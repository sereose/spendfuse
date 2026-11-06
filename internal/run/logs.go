package run

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"

	"github.com/sereose/spendfuse/internal/logsinsights"
	"github.com/sereose/spendfuse/internal/notify"
)

// LogsOptions tunes the Logs Insights signal.
type LogsOptions struct {
	Window              time.Duration
	MaxUSDPerHour       float64
	MaxProjectedMonthly float64
	SlackWebhook        string
	DryRun              bool
	Days                int
	TopN                int
}

// LogsWatch evaluates recent Logs Insights queries.
func LogsWatch(ctx context.Context, c *Clients, o LogsOptions) (string, bool, error) {
	w := &logsinsights.Watcher{Logs: cloudwatchlogs.NewFromConfig(c.cfg), PricePerGB: c.LogsPrice.PerGB}
	r, err := w.Watch(ctx, o.Window)
	if err != nil {
		return "", false, err
	}
	v := logsinsights.Evaluate(r, o.MaxUSDPerHour, o.MaxProjectedMonthly)

	var b strings.Builder
	fmt.Fprintf(&b, "spendfuse · Logs Insights · %s · $%.4f/GB scanned (%s)\n", c.Region, c.LogsPrice.PerGB, c.LogsPrice.Source)
	fmt.Fprintf(&b, "window %s: %d queries, %.1f GB scanned, $%.2f → burn $%.2f/h · limits: $%.2f/h, $%.0f/month per pattern\n",
		o.Window, r.Queries, float64(r.Bytes)/(1<<30), r.USD, r.BurnPerHour, o.MaxUSDPerHour, o.MaxProjectedMonthly)
	if r.Truncated {
		b.WriteString("WARNING: query history was cut off by the API; the window is under-counted.\n")
	}
	if len(r.Patterns) == 0 {
		b.WriteString("\nno Logs Insights queries in this window\n")
		return b.String(), false, nil
	}
	top := o.TopN
	if top <= 0 || top > len(r.Patterns) {
		top = len(r.Patterns)
	}
	fmt.Fprintf(&b, "\n%-6s %-10s %-9s %9s %8s %12s  %-32s %s\n", "RUNS", "INTERVAL", "GB", "USD", "FLAG", "PROJ $/mo", "LOG GROUP", "QUERY")
	for _, p := range r.Patterns[:top] {
		interval := "—"
		if p.AvgInterval > 0 {
			interval = p.AvgInterval.Round(time.Second).String()
		}
		flag := ""
		if p.AutoRefresh {
			flag = "REFRESH"
		}
		proj := fmt.Sprintf("%.0f", p.ProjectedUSD)
		if math.IsInf(p.ProjectedUSD, 0) {
			proj = "∞"
		}
		fmt.Fprintf(&b, "%-6d %-10s %9.1f %9.2f %8s %12s  %-32s %s\n",
			p.Runs, interval, float64(p.Bytes)/(1<<30), p.USD, flag, proj, trunc(p.LogGroup, 32), logsinsights.Short(p.Text, 70))
	}

	if v.Breach {
		b.WriteString("\nBREACH:\n")
		for _, r := range v.Reasons {
			b.WriteString("  • " + r + "\n")
		}
		msg := fmt.Sprintf(":rotating_light: *spendfuse* Logs Insights spend breach in %s\n• %s\n_fix: narrow the dashboard time range, disable auto-refresh, or replace the widget with a metric filter. action: notify only (prototype)_",
			c.Region, strings.Join(v.Reasons, "\n• "))
		if o.DryRun {
			b.WriteString("\n[dry-run] would notify Slack:\n" + msg + "\n")
		} else if err := notify.Slack(ctx, o.SlackWebhook, msg); err != nil {
			fmt.Fprintf(&b, "\nslack notify failed: %v\n", err)
		} else if o.SlackWebhook != "" {
			b.WriteString("\nSlack notified.\n")
		}
	}
	return b.String(), v.Breach, nil
}

// LogsCalibrate compares scanned bytes we observed with Cost Explorer.
func LogsCalibrate(ctx context.Context, c *Clients, o LogsOptions) (string, error) {
	w := &logsinsights.Watcher{Logs: cloudwatchlogs.NewFromConfig(c.cfg), PricePerGB: c.LogsPrice.PerGB}
	rs, truncated, err := w.Calibrate(ctx, c.CE, o.Days)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "spendfuse · Logs Insights calibration · %s · $%.4f/GB (%s)\n", c.Region, c.LogsPrice.PerGB, c.LogsPrice.Source)
	b.WriteString("Compares BytesScanned from DescribeQueries (this region) with Cost Explorer DataScanned-Bytes (whole account), per UTC day.\n\n")
	fmt.Fprintf(&b, "%-12s %8s %10s %10s | %10s %10s | %8s\n", "DAY", "QUERIES", "EST GB", "BILLED GB", "EST USD", "BILL USD", "ERROR")
	for _, r := range rs {
		errs := fmt.Sprintf("%+.1f%%", r.ErrorPct)
		if math.IsInf(r.ErrorPct, 1) {
			errs = "n/a"
		}
		fmt.Fprintf(&b, "%-12s %8d %10.2f %10.2f | %10.3f %10.3f | %8s\n", r.Day, r.Queries, r.EstGB, r.BilledGB, r.EstUSD, r.BilledUSD, errs)
	}
	b.WriteString("\n" + logsinsights.Summary(rs, truncated) + "\n")
	return b.String(), nil
}
