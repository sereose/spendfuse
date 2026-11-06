package logsinsights

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

type fakeLogs struct{ qs []cwltypes.QueryInfo }

func (f fakeLogs) DescribeQueries(context.Context, *cloudwatchlogs.DescribeQueriesInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeQueriesOutput, error) {
	return &cloudwatchlogs.DescribeQueriesOutput{Queries: f.qs}, nil
}

func q(id, text, lg string, at time.Time, bytes float64) cwltypes.QueryInfo {
	return cwltypes.QueryInfo{
		QueryId: aws.String(id), QueryString: aws.String(text), LogGroupName: aws.String(lg),
		CreateTime: aws.Int64(at.UnixMilli()), BytesScanned: aws.Float64(bytes), Status: cwltypes.QueryStatusComplete,
	}
}

func TestAnalyzeFindsAutoRefreshDashboard(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var qs []cwltypes.QueryInfo
	// A dashboard widget: same query every 60s for an hour, 50 GiB each → 60 runs, 3000 GiB.
	for i := 0; i < 60; i++ {
		qs = append(qs, q("d"+string(rune('a'+i%26)), "fields @timestamp, @message\n| filter @message like /ERROR/", "/aws/lambda/api",
			now.Add(-time.Duration(60-i)*time.Minute), 50*gib))
	}
	// A human: one ad-hoc query, 2 GiB.
	qs = append(qs, q("h1", "stats count() by bin(5m)", "/aws/ecs/web", now.Add(-10*time.Minute), 2*gib))
	// Old query outside the window: ignored.
	qs = append(qs, q("old", "fields @message", "/old", now.Add(-3*time.Hour), 999*gib))

	w := &Watcher{Logs: fakeLogs{qs}, PricePerGB: 0.005, Now: func() time.Time { return now }}
	r, err := w.Watch(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if r.Queries != 61 {
		t.Fatalf("queries in window = %d, want 61", r.Queries)
	}
	// 3002 GiB × $0.005 = $15.01 in one hour.
	if math.Abs(r.USD-15.01) > 0.01 || math.Abs(r.BurnPerHour-15.01) > 0.01 {
		t.Errorf("usd=%.3f burn=%.3f", r.USD, r.BurnPerHour)
	}
	top := r.Patterns[0]
	if top.Runs != 60 || !top.AutoRefresh || top.LogGroup != "/aws/lambda/api" {
		t.Errorf("top pattern = %+v", top)
	}
	if math.Abs(top.AvgInterval.Minutes()-1) > 0.01 {
		t.Errorf("avg interval = %s, want 1m", top.AvgInterval)
	}
	// Projection: $0.25/run × (30d / 1m = 43200 runs) = $10,800/month.
	if math.Abs(top.ProjectedUSD-10800) > 1 {
		t.Errorf("projected = %.0f, want 10800", top.ProjectedUSD)
	}
	v := Evaluate(r, 0, 500)
	if !v.Breach || len(v.Reasons) != 1 || !strings.Contains(v.Reasons[0], "auto-refreshing") {
		t.Errorf("verdict = %+v", v)
	}
	// The human's single query must not trip the projected-monthly limit (Runs < 2).
	if v2 := Evaluate(&Report{Patterns: []Pattern{{Runs: 1, ProjectedUSD: 1e9}}}, 0, 500); v2.Breach {
		t.Error("single run must not breach projection limit")
	}
}

func TestHashNormalisesWhitespaceAndCase(t *testing.T) {
	a := hashQuery("fields @message\n|  filter x", "/g")
	b := hashQuery("FIELDS @message | filter x", "/g")
	c := hashQuery("fields @message | filter x", "/other")
	if a != b || a == c {
		t.Errorf("hash: a=%s b=%s c=%s", a, b, c)
	}
}

type fakeCE struct{ gbPerDay float64 }

func (f fakeCE) GetCostAndUsage(_ context.Context, in *costexplorerInput, _ ...func(*costexplorerOptions)) (*costexplorerOutput, error) {
	return ceFake(f.gbPerDay, in)
}

func TestCalibrateAgainstCostExplorer(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	var qs []cwltypes.QueryInfo
	// 100 GiB scanned on each of 11-03, 11-04 (11-05 is "yesterday", excluded).
	for _, d := range []int{3, 4, 5} {
		qs = append(qs, q("x", "fields @message", "/g", time.Date(2026, 11, d, 10, 0, 0, 0, time.UTC), 100*gib))
	}
	w := &Watcher{Logs: fakeLogs{qs}, PricePerGB: 0.005, Now: func() time.Time { return now }}
	rs, truncated, err := w.Calibrate(context.Background(), fakeCE{gbPerDay: 100}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("should not be truncated with 3 queries")
	}
	if len(rs) != 2 || rs[0].Day != "2026-11-03" || rs[1].Day != "2026-11-04" {
		t.Fatalf("days = %+v", rs)
	}
	for _, r := range rs {
		if r.Queries != 1 || math.Abs(r.ErrorPct) > 0.5 {
			t.Errorf("%s: queries=%d est=%.3f billed=%.3f err=%.2f%%", r.Day, r.Queries, r.EstUSD, r.BilledUSD, r.ErrorPct)
		}
	}
	if s := Summary(rs, false); !strings.Contains(s, "excellent") {
		t.Errorf("summary: %s", s)
	}
}

func TestParsePriceList(t *testing.T) {
	items := []string{
		`{"product":{"attributes":{"usagetype":"EUC1-DataProcessing-Bytes"}},"terms":{"OnDemand":{"a":{"priceDimensions":{"b":{"pricePerUnit":{"USD":"0.57"}}}}}}}`,
		`{"product":{"attributes":{"usagetype":"EUC1-DataScanned-Bytes"}},"terms":{"OnDemand":{"a":{"priceDimensions":{"b":{"pricePerUnit":{"USD":"0.0063000000"}}}}}}}`,
	}
	p, ok := parsePriceList(items, "eu-central-1")
	if !ok || p.PerGB != 0.0063 {
		t.Fatalf("parse = %+v ok=%v", p, ok)
	}
	if fb := LookupPrice(context.Background(), nil, "ap-northeast-1"); fb.PerGB != 0.0076 || fb.Source != "fallback" {
		t.Errorf("fallback = %+v", fb)
	}
}
