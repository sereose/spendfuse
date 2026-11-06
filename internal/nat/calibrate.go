package nat

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

// CostExplorerAPI is the client subset used for calibration.
type CostExplorerAPI interface {
	GetCostAndUsage(ctx context.Context, in *costexplorer.GetCostAndUsageInput, opts ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error)
}

// DayResult compares our estimate with the bill for one UTC day.
type DayResult struct {
	Day            string
	EstDataUSD     float64
	EstHourlyUSD   float64
	EstTotalUSD    float64
	EstGB          float64
	BilledDataUSD  float64
	BilledHoursUSD float64
	BilledTotalUSD float64
	BilledGB       float64
	ErrorPct       float64 // (est - billed) / billed * 100; +∞ if billed is 0 and est > 0
}

// Calibrate estimates NAT cost from CloudWatch for each of the last `days`
// complete UTC days and pulls Cost Explorer's actuals for the same days.
// Cost Explorer lags ~24h, so the most recent day may be partial; we start
// from two days ago to be safe.
func (w *Watcher) Calibrate(ctx context.Context, ce CostExplorerAPI, days int) ([]DayResult, error) {
	if days < 1 {
		days = 1
	}
	gws, err := w.Gateways(ctx)
	if err != nil {
		return nil, err
	}
	today := w.now().UTC().Truncate(24 * time.Hour)
	end := today.AddDate(0, 0, -1) // exclusive; yesterday is usually incomplete in CE
	start := end.AddDate(0, 0, -days)

	// Our side: one bucket per day per gateway, summed.
	bytesPerDay := make([]int64, days)
	if len(gws) > 0 {
		per, err := w.ProcessedBytes(ctx, gws, start, end, 24*time.Hour)
		if err != nil {
			return nil, err
		}
		for _, series := range per {
			for i, v := range series {
				if i < days {
					bytesPerDay[i] += v
				}
			}
		}
	}
	hoursPerDay := 24.0 * float64(len(gws))

	// AWS side.
	billed, err := natActuals(ctx, ce, start, end)
	if err != nil {
		return nil, err
	}

	var out []DayResult
	for i := 0; i < days; i++ {
		day := start.AddDate(0, 0, i).Format("2006-01-02")
		r := DayResult{Day: day}
		r.EstGB = float64(bytesPerDay[i]) / gib
		r.EstDataUSD = r.EstGB * w.Price.PerGB
		r.EstHourlyUSD = hoursPerDay * w.Price.PerHour
		r.EstTotalUSD = r.EstDataUSD + r.EstHourlyUSD
		if a, ok := billed[day]; ok {
			r.BilledDataUSD, r.BilledHoursUSD, r.BilledGB = a.dataUSD, a.hoursUSD, a.gb
			r.BilledTotalUSD = a.dataUSD + a.hoursUSD
		}
		r.ErrorPct = pctError(r.EstTotalUSD, r.BilledTotalUSD)
		out = append(out, r)
	}
	return out, nil
}

type actual struct {
	dataUSD, hoursUSD, gb float64
}

// natActuals queries Cost Explorer for NAT usage-type groups, daily.
func natActuals(ctx context.Context, ce CostExplorerAPI, start, end time.Time) (map[string]actual, error) {
	res := map[string]actual{}
	if ce == nil {
		return res, nil
	}
	var next *string
	for {
		out, err := ce.GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
			TimePeriod: &cetypes.DateInterval{
				Start: aws.String(start.Format("2006-01-02")),
				End:   aws.String(end.Format("2006-01-02")),
			},
			Granularity: cetypes.GranularityDaily,
			Metrics:     []string{"UnblendedCost", "UsageQuantity"},
			Filter: &cetypes.Expression{Dimensions: &cetypes.DimensionValues{
				Key: cetypes.DimensionUsageTypeGroup,
				Values: []string{
					"EC2: NAT Gateway - Data Processed",
					"EC2: NAT Gateway - Running Hours",
				},
			}},
			GroupBy:       []cetypes.GroupDefinition{{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("USAGE_TYPE_GROUP")}},
			NextPageToken: next,
		})
		if err != nil {
			return nil, fmt.Errorf("cost explorer: %w", err)
		}
		for _, rt := range out.ResultsByTime {
			day := aws.ToString(rt.TimePeriod.Start)
			a := res[day]
			for _, g := range rt.Groups {
				cost := metric(g.Metrics, "UnblendedCost")
				qty := metric(g.Metrics, "UsageQuantity")
				key := strings.Join(g.Keys, "")
				switch {
				case strings.Contains(key, "Data Processed"):
					a.dataUSD += cost
					a.gb += qty
				case strings.Contains(key, "Running Hours"):
					a.hoursUSD += cost
				}
			}
			res[day] = a
		}
		if out.NextPageToken == nil {
			break
		}
		next = out.NextPageToken
	}
	return res, nil
}

func metric(m map[string]cetypes.MetricValue, name string) float64 {
	v, ok := m[name]
	if !ok || v.Amount == nil {
		return 0
	}
	f, _ := strconv.ParseFloat(*v.Amount, 64)
	return f
}

func pctError(est, billed float64) float64 {
	switch {
	case billed == 0 && est == 0:
		return 0
	case billed == 0:
		return math.Inf(1)
	default:
		return (est - billed) / billed * 100
	}
}

// Summary aggregates calibration days into one verdict line. When the bill is
// $0 (free tier) but usage quantity is present, it compares GB instead.
func Summary(rs []DayResult) string {
	var est, billed float64
	n := 0
	for _, r := range rs {
		if r.BilledTotalUSD > 0 {
			est += r.EstTotalUSD
			billed += r.BilledTotalUSD
			n++
		}
	}
	unit := "$"
	if n == 0 {
		// Fall back to data quantity (GB processed), which the bill reports even at $0.
		for _, r := range rs {
			if r.BilledGB > 0 {
				est += r.EstGB
				billed += r.BilledGB
				n++
			}
		}
		unit = "GB"
	}
	if n == 0 {
		return "no billed NAT usage in Cost Explorer for these days — nothing to calibrate against (new account, no NAT, or CE lag)"
	}
	e := pctError(est, billed)
	if unit == "GB" {
		return fmt.Sprintf("%d day(s): billed cost is $0 (free tier?) — comparing data volume instead: estimated %.2f GB vs billed %.2f GB → error %+.1f%%", n, est, billed, e)
	}
	grade := "excellent (<5%)"
	switch {
	case math.Abs(e) > 25:
		grade = "poor (>25%) — do not ship thresholds on this estimate without investigating"
	case math.Abs(e) > 10:
		grade = "acceptable (10–25%) — use thresholds with margin"
	case math.Abs(e) > 5:
		grade = "good (5–10%)"
	}
	return fmt.Sprintf("%d day(s): estimated $%.2f vs billed $%.2f → error %+.1f%% — %s", n, est, billed, e, grade)
}
