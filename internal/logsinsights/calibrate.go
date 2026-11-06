package logsinsights

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

// DayResult compares scanned bytes we saw with what Cost Explorer billed.
type DayResult struct {
	Day       string
	Queries   int
	EstGB     float64
	EstUSD    float64
	BilledGB  float64
	BilledUSD float64
	ErrorPct  float64
}

// Calibrate compares DescribeQueries-derived scan volume with Cost Explorer's
// "DataScanned-Bytes" usage, per complete UTC day. The API keeps a bounded
// query history, so on busy accounts only the most recent day(s) are complete;
// the report says how many queries each day was built from.
func (w *Watcher) Calibrate(ctx context.Context, ce CostExplorerAPI, days int) ([]DayResult, bool, error) {
	if days < 1 {
		days = 1
	}
	today := w.now().UTC().Truncate(24 * time.Hour)
	end := today.AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -days)

	qs, truncated, err := w.Recent(ctx, start)
	if err != nil {
		return nil, false, err
	}
	perDayBytes := map[string]int64{}
	perDayCount := map[string]int{}
	for _, q := range qs {
		if !q.CreatedAt.Before(end) {
			continue
		}
		d := q.CreatedAt.UTC().Format("2006-01-02")
		perDayBytes[d] += q.BytesScanned
		perDayCount[d]++
	}

	billed, err := scanActuals(ctx, ce, start, end)
	if err != nil {
		return nil, false, err
	}

	var out []DayResult
	for i := 0; i < days; i++ {
		day := start.AddDate(0, 0, i).Format("2006-01-02")
		r := DayResult{Day: day, Queries: perDayCount[day]}
		r.EstGB = float64(perDayBytes[day]) / gib
		r.EstUSD = r.EstGB * w.PricePerGB
		if a, ok := billed[day]; ok {
			r.BilledGB, r.BilledUSD = a.gb, a.usd
		}
		r.ErrorPct = pctError(r.EstUSD, r.BilledUSD)
		out = append(out, r)
	}
	return out, truncated, nil
}

type actual struct{ gb, usd float64 }

// scanActuals sums CloudWatch usage types ending in DataScanned-Bytes per day.
func scanActuals(ctx context.Context, ce CostExplorerAPI, start, end time.Time) (map[string]actual, error) {
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
				Key: cetypes.DimensionService, Values: []string{"AmazonCloudWatch"},
			}},
			GroupBy:       []cetypes.GroupDefinition{{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("USAGE_TYPE")}},
			NextPageToken: next,
		})
		if err != nil {
			return nil, fmt.Errorf("cost explorer: %w", err)
		}
		for _, rt := range out.ResultsByTime {
			day := aws.ToString(rt.TimePeriod.Start)
			a := res[day]
			for _, g := range rt.Groups {
				if !strings.HasSuffix(strings.Join(g.Keys, ""), "DataScanned-Bytes") {
					continue
				}
				a.usd += metric(g.Metrics, "UnblendedCost")
				a.gb += metric(g.Metrics, "UsageQuantity")
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

// Summary gives one verdict line.
func Summary(rs []DayResult, truncated bool) string {
	var est, billed float64
	n := 0
	for _, r := range rs {
		if r.BilledUSD > 0 {
			est += r.EstUSD
			billed += r.BilledUSD
			n++
		}
	}
	unit := "$"
	if n == 0 {
		// Free tier: first 5 GB scanned/month bill at $0 but usage quantity is reported.
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
		return "no billed Logs Insights scanning in Cost Explorer for these days — nothing to calibrate against"
	}
	e := pctError(est, billed)
	if unit == "GB" {
		s := fmt.Sprintf("%d day(s): billed cost is $0 (free tier) — comparing scanned volume instead: estimated %.2f GB vs billed %.2f GB → error %+.1f%%", n, est, billed, e)
		if truncated {
			s += "\nWARNING: DescribeQueries history was cut off; older days are under-counted."
		}
		return s
	}
	grade := "excellent (<5%)"
	switch {
	case math.Abs(e) > 25:
		grade = "poor (>25%)"
	case math.Abs(e) > 10:
		grade = "acceptable (10–25%)"
	case math.Abs(e) > 5:
		grade = "good (5–10%)"
	}
	s := fmt.Sprintf("%d day(s): estimated $%.2f vs billed $%.2f → error %+.1f%% — %s", n, est, billed, e, grade)
	if truncated {
		s += "\nWARNING: DescribeQueries history was cut off; older days are under-counted. Trust only the most recent day(s)."
	}
	if e < -25 {
		s += "\nLarge under-estimate usually means queries ran in other regions (Cost Explorer is account-wide) or history was truncated."
	}
	return s
}
