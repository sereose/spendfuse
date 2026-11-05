// Package nat implements the NAT Gateway spend signal: near-real-time cost
// estimation from CloudWatch byte counters, and calibration of that estimate
// against what Cost Explorer later bills.
package nat

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

const gib = 1024 * 1024 * 1024

// Billable bytes: everything that enters the gateway, from either side.
// (BytesOut* mirror BytesIn* and would double-count.)
var processedMetrics = []string{"BytesInFromSource", "BytesInFromDestination"}

// EC2API and CloudWatchAPI are the client subsets we need; fakes implement them in tests.
type EC2API interface {
	DescribeNatGateways(ctx context.Context, in *ec2.DescribeNatGatewaysInput, opts ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error)
}

type CloudWatchAPI interface {
	GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// Gateway is a NAT gateway we watch.
type Gateway struct {
	ID    string
	VPC   string
	State string
	Name  string
}

// Sample is the cost picture for one gateway over one window.
type Sample struct {
	Gateway       Gateway
	Window        time.Duration
	Bytes         int64
	GB            float64
	DataUSD       float64 // data processing cost inside the window
	HourlyUSD     float64 // gateway-hour cost inside the window
	BurnPerHour   float64 // extrapolated $/h right now (data + hourly)
	BaselinePerHr float64 // average $/h over the baseline period
	Ratio         float64 // BurnPerHour / BaselinePerHr (0 if no baseline)
}

// Watcher pulls gateways and metrics.
type Watcher struct {
	EC2   EC2API
	CW    CloudWatchAPI
	Price Price
	Now   func() time.Time
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Gateways lists available NAT gateways in the region.
func (w *Watcher) Gateways(ctx context.Context) ([]Gateway, error) {
	var out []Gateway
	var token *string
	for {
		res, err := w.EC2.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{NextToken: token})
		if err != nil {
			return nil, fmt.Errorf("describe nat gateways: %w", err)
		}
		for _, g := range res.NatGateways {
			gw := Gateway{ID: aws.ToString(g.NatGatewayId), VPC: aws.ToString(g.VpcId), State: string(g.State)}
			for _, t := range g.Tags {
				if aws.ToString(t.Key) == "Name" {
					gw.Name = aws.ToString(t.Value)
				}
			}
			if gw.State == "available" {
				out = append(out, gw)
			}
		}
		if res.NextToken == nil {
			break
		}
		token = res.NextToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ProcessedBytes returns billable bytes per gateway between start and end,
// bucketed by period. Result: gatewayID -> []bytes per bucket (chronological).
func (w *Watcher) ProcessedBytes(ctx context.Context, gws []Gateway, start, end time.Time, period time.Duration) (map[string][]int64, error) {
	result := map[string][]int64{}
	if len(gws) == 0 {
		return result, nil
	}
	// GetMetricData allows 500 queries per call; 2 metrics per gateway.
	const batch = 200
	for i := 0; i < len(gws); i += batch {
		j := i + batch
		if j > len(gws) {
			j = len(gws)
		}
		if err := w.fetchBatch(ctx, gws[i:j], start, end, period, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (w *Watcher) fetchBatch(ctx context.Context, gws []Gateway, start, end time.Time, period time.Duration, result map[string][]int64) error {
	var queries []cwtypes.MetricDataQuery
	idx := map[string]string{} // query id -> gateway id
	for n, g := range gws {
		for m, metric := range processedMetrics {
			id := fmt.Sprintf("q%d_%d", n, m)
			idx[id] = g.ID
			queries = append(queries, cwtypes.MetricDataQuery{
				Id: aws.String(id),
				MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{
						Namespace:  aws.String("AWS/NATGateway"),
						MetricName: aws.String(metric),
						Dimensions: []cwtypes.Dimension{{Name: aws.String("NatGatewayId"), Value: aws.String(g.ID)}},
					},
					Period: aws.Int32(int32(period.Seconds())),
					Stat:   aws.String("Sum"),
				},
				ReturnData: aws.Bool(true),
			})
		}
	}

	buckets := int(end.Sub(start) / period)
	if buckets < 1 {
		buckets = 1
	}
	for _, g := range gws {
		if _, ok := result[g.ID]; !ok {
			result[g.ID] = make([]int64, buckets)
		}
	}

	var next *string
	for {
		res, err := w.CW.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
			StartTime:         aws.Time(start),
			EndTime:           aws.Time(end),
			MetricDataQueries: queries,
			ScanBy:            cwtypes.ScanByTimestampAscending,
			NextToken:         next,
		})
		if err != nil {
			return fmt.Errorf("get metric data: %w", err)
		}
		for _, r := range res.MetricDataResults {
			gid := idx[aws.ToString(r.Id)]
			series := result[gid]
			for k, ts := range r.Timestamps {
				b := int(ts.Sub(start) / period)
				if b < 0 || b >= len(series) {
					continue
				}
				series[b] += int64(r.Values[k])
			}
		}
		if res.NextToken == nil {
			break
		}
		next = res.NextToken
	}
	return nil
}

// Estimate turns bytes over a window into dollars.
func Estimate(p Price, bytes int64, window time.Duration) (dataUSD, hourlyUSD float64) {
	gb := float64(bytes) / gib
	return gb * p.PerGB, p.PerHour * window.Hours()
}

// Watch computes the current burn rate per gateway and compares to a baseline.
func (w *Watcher) Watch(ctx context.Context, window, baseline time.Duration) ([]Sample, error) {
	gws, err := w.Gateways(ctx)
	if err != nil {
		return nil, err
	}
	now := w.now().Truncate(time.Minute)
	// CloudWatch NAT metrics lag ~1–3 minutes; step back so the window is complete.
	end := now.Add(-2 * time.Minute)

	cur, err := w.ProcessedBytes(ctx, gws, end.Add(-window), end, window)
	if err != nil {
		return nil, err
	}
	base, err := w.ProcessedBytes(ctx, gws, end.Add(-baseline), end, baseline)
	if err != nil {
		return nil, err
	}

	var out []Sample
	for _, g := range gws {
		var b int64
		for _, v := range cur[g.ID] {
			b += v
		}
		data, hourly := Estimate(w.Price, b, window)
		s := Sample{Gateway: g, Window: window, Bytes: b, GB: float64(b) / gib, DataUSD: data, HourlyUSD: hourly}
		s.BurnPerHour = (data + hourly) / window.Hours()

		var bb int64
		for _, v := range base[g.ID] {
			bb += v
		}
		bdata, bhourly := Estimate(w.Price, bb, baseline)
		s.BaselinePerHr = (bdata + bhourly) / baseline.Hours()
		if s.BaselinePerHr > 0 {
			s.Ratio = s.BurnPerHour / s.BaselinePerHr
		}
		out = append(out, s)
	}
	return out, nil
}

// Verdict decides whether a sample breaches the policy.
type Verdict struct {
	Breach bool
	Reason string
}

// Evaluate applies two independent limits: absolute $/h and multiple of baseline.
func Evaluate(s Sample, maxUSDPerHour, maxRatio float64) Verdict {
	if maxUSDPerHour > 0 && s.BurnPerHour > maxUSDPerHour {
		return Verdict{true, fmt.Sprintf("burn $%.2f/h exceeds limit $%.2f/h", s.BurnPerHour, maxUSDPerHour)}
	}
	if maxRatio > 0 && s.Ratio > maxRatio && s.BurnPerHour > 0.5 { // ignore noise on idle gateways
		return Verdict{true, fmt.Sprintf("burn $%.2f/h is %.1f× the 24h baseline ($%.2f/h)", s.BurnPerHour, s.Ratio, s.BaselinePerHr)}
	}
	return Verdict{}
}
