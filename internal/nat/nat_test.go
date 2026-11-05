package nat

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ---- fakes -----------------------------------------------------------------

type fakeEC2 struct{ gws []string }

func (f fakeEC2) DescribeNatGateways(context.Context, *ec2.DescribeNatGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error) {
	var out ec2.DescribeNatGatewaysOutput
	for _, id := range f.gws {
		out.NatGateways = append(out.NatGateways, ec2types.NatGateway{
			NatGatewayId: aws.String(id), VpcId: aws.String("vpc-1"), State: ec2types.NatGatewayStateAvailable,
			Tags: []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("nat-" + id)}},
		})
	}
	// one deleted gateway must be ignored
	out.NatGateways = append(out.NatGateways, ec2types.NatGateway{NatGatewayId: aws.String("nat-gone"), State: ec2types.NatGatewayStateDeleted})
	return &out, nil
}

// fakeCW returns, for every query, `perBucket` bytes in each period bucket.
type fakeCW struct {
	perBucket float64
	calls     int
}

func (f *fakeCW) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	f.calls++
	start, end := *in.StartTime, *in.EndTime
	var out cloudwatch.GetMetricDataOutput
	for _, q := range in.MetricDataQueries {
		period := time.Duration(*q.MetricStat.Period) * time.Second
		r := cwtypes.MetricDataResult{Id: q.Id}
		for t := start; t.Before(end); t = t.Add(period) {
			r.Timestamps = append(r.Timestamps, t)
			r.Values = append(r.Values, f.perBucket)
		}
		out.MetricDataResults = append(out.MetricDataResults, r)
	}
	return &out, nil
}

type fakeCE struct{ perDayGB float64 }

func (f fakeCE) GetCostAndUsage(_ context.Context, in *costexplorer.GetCostAndUsageInput, _ ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error) {
	start, _ := time.Parse("2006-01-02", *in.TimePeriod.Start)
	end, _ := time.Parse("2006-01-02", *in.TimePeriod.End)
	var out costexplorer.GetCostAndUsageOutput
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		out.ResultsByTime = append(out.ResultsByTime, cetypes.ResultByTime{
			TimePeriod: &cetypes.DateInterval{Start: aws.String(d.Format("2006-01-02"))},
			Groups: []cetypes.Group{
				{Keys: []string{"EC2: NAT Gateway - Data Processed"}, Metrics: map[string]cetypes.MetricValue{
					"UnblendedCost": {Amount: aws.String(fmtF(f.perDayGB * 0.045))},
					"UsageQuantity": {Amount: aws.String(fmtF(f.perDayGB))},
				}},
				{Keys: []string{"EC2: NAT Gateway - Running Hours"}, Metrics: map[string]cetypes.MetricValue{
					"UnblendedCost": {Amount: aws.String(fmtF(24 * 0.045))},
					"UsageQuantity": {Amount: aws.String("24")},
				}},
			},
		})
	}
	return &out, nil
}

func fmtF(f float64) string { return aws.ToString(aws.String(trimFloat(f))) }

func trimFloat(f float64) string {
	s := make([]byte, 0, 24)
	return string(appendFloat(s, f))
}

// ---- tests -----------------------------------------------------------------

func TestEstimate(t *testing.T) {
	p := Price{PerHour: 0.045, PerGB: 0.045}
	data, hourly := Estimate(p, 10*gib, time.Hour)
	if math.Abs(data-0.45) > 1e-9 || math.Abs(hourly-0.045) > 1e-9 {
		t.Fatalf("Estimate = %.4f, %.4f", data, hourly)
	}
}

func TestWatchComputesBurnAndRatio(t *testing.T) {
	// 1 GiB per minute now → in a 5m window: 5 GiB → $0.225 data + $0.00375 hourly → $2.745/h
	// baseline fake returns the same per-bucket value for a 24h single bucket: 1 GiB/24h → tiny baseline.
	cw := &fakeCW{perBucket: float64(gib)}
	w := &Watcher{
		EC2:   fakeEC2{gws: []string{"nat-a"}},
		CW:    cw,
		Price: Price{PerHour: 0.045, PerGB: 0.045},
		Now:   func() time.Time { return time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC) },
	}
	// Watch uses one bucket for the window and one for the baseline; the fake
	// yields 1 GiB per metric, and there are 2 billable metrics → 2 GiB each.
	// window:   (2×0.045 + 0.045×5/60) / (5/60) = 1.125 $/h
	// baseline: (2×0.045 + 0.045×24) / 24      = 0.0488 $/h → ratio ≈ 23
	samples, err := w.Watch(context.Background(), 5*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Gateway.ID != "nat-a" {
		t.Fatalf("samples = %+v", samples)
	}
	s := samples[0]
	if math.Abs(s.BurnPerHour-1.125) > 0.001 {
		t.Errorf("burn = %.3f, want 1.125", s.BurnPerHour)
	}
	if math.Abs(s.Ratio-23.08) > 0.1 {
		t.Errorf("ratio = %.2f, want ≈23.08", s.Ratio)
	}
	if cw.calls != 2 {
		t.Errorf("expected 2 GetMetricData calls (window + baseline), got %d", cw.calls)
	}
	v := Evaluate(s, 0, 3)
	if !v.Breach {
		t.Errorf("expected ratio breach, got %+v", v)
	}
	if v := Evaluate(s, 100, 0); v.Breach {
		t.Errorf("absolute limit 100 must not breach at $0.585/h: %+v", v)
	}
}

func TestEvaluateIgnoresNoiseOnIdleGateway(t *testing.T) {
	s := Sample{BurnPerHour: 0.2, BaselinePerHr: 0.01, Ratio: 20}
	if Evaluate(s, 0, 3).Breach {
		t.Error("ratio breach on <$0.5/h burn should be suppressed")
	}
}

func TestCalibrateMatchesWhenSourcesAgree(t *testing.T) {
	// CloudWatch: 2 metrics × 100 GiB per daily bucket = 200 GiB/day per gateway.
	// Cost Explorer fake bills 200 GB/day → estimate and bill agree except GiB vs GB.
	cw := &fakeCW{perBucket: 100 * float64(gib)}
	w := &Watcher{
		EC2:   fakeEC2{gws: []string{"nat-a"}},
		CW:    cw,
		Price: Price{PerHour: 0.045, PerGB: 0.045},
		Now:   func() time.Time { return time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC) },
	}
	rs, err := w.Calibrate(context.Background(), fakeCE{perDayGB: 200}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("days = %d", len(rs))
	}
	// Days must be 2026-11-01 .. 2026-11-03 (yesterday 11-04 excluded).
	if rs[0].Day != "2026-11-01" || rs[2].Day != "2026-11-03" {
		t.Errorf("day range wrong: %s..%s", rs[0].Day, rs[2].Day)
	}
	for _, r := range rs {
		if math.Abs(r.ErrorPct) > 1 {
			t.Errorf("%s: error %.2f%% — estimate %.2f vs billed %.2f", r.Day, r.ErrorPct, r.EstTotalUSD, r.BilledTotalUSD)
		}
	}
	if got := Summary(rs); !contains(got, "excellent") {
		t.Errorf("summary: %s", got)
	}
}

func TestCalibrateNoBilling(t *testing.T) {
	w := &Watcher{EC2: fakeEC2{}, CW: &fakeCW{}, Price: Price{PerHour: 0.045, PerGB: 0.045}}
	rs, err := w.Calibrate(context.Background(), nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := Summary(rs); !contains(got, "nothing to calibrate") {
		t.Errorf("summary: %s", got)
	}
}

func TestParsePriceList(t *testing.T) {
	items := []string{
		`{"product":{"attributes":{"usagetype":"EUC1-NatGateway-Hours"}},"terms":{"OnDemand":{"X":{"priceDimensions":{"Y":{"unit":"Hrs","pricePerUnit":{"USD":"0.0520000000"}}}}}}}`,
		`{"product":{"attributes":{"usagetype":"EUC1-NatGateway-Bytes"}},"terms":{"OnDemand":{"X":{"priceDimensions":{"Y":{"unit":"GB","pricePerUnit":{"USD":"0.0520000000"}}}}}}}`,
	}
	p, ok := parsePriceList(items, "eu-central-1")
	if !ok || p.PerHour != 0.052 || p.PerGB != 0.052 || p.Source != "pricing-api" {
		t.Fatalf("parse = %+v ok=%v", p, ok)
	}
	if _, ok := parsePriceList([]string{`{}`}, "x"); ok {
		t.Error("empty price list must not parse")
	}
	fb := LookupPrice(context.Background(), nil, "sa-east-1")
	if fb.Source != "fallback" || fb.PerGB != 0.093 {
		t.Errorf("fallback = %+v", fb)
	}
}

func TestPctError(t *testing.T) {
	if pctError(110, 100) != 10 || pctError(0, 0) != 0 || !math.IsInf(pctError(1, 0), 1) {
		t.Error("pctError")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
