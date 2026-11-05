// Package run wires AWS clients to the NAT signal and formats output. Shared
// by the CLI and the Lambda handler.
package run

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/pricing"

	"github.com/sereose/spendfuse/internal/nat"
	"github.com/sereose/spendfuse/internal/notify"
)

// Options for one run.
type Options struct {
	Region        string
	Profile       string
	Window        time.Duration
	Baseline      time.Duration
	MaxUSDPerHour float64
	MaxRatio      float64
	SlackWebhook  string
	DryRun        bool
	Days          int // calibrate only
}

// Clients bundles what the signal needs.
type Clients struct {
	Region string
	EC2    *ec2.Client
	CW     *cloudwatch.Client
	CE     *costexplorer.Client
	Price  nat.Price
}

// Connect loads AWS config and resolves the NAT price for the region.
func Connect(ctx context.Context, o Options) (*Clients, error) {
	var loadOpts []func(*config.LoadOptions) error
	if o.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(o.Region))
	}
	if o.Profile != "" {
		loadOpts = append(loadOpts, config.WithSharedConfigProfile(o.Profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("no AWS region: pass --region or set AWS_REGION")
	}
	// Pricing and Cost Explorer are global services served from us-east-1.
	global := cfg.Copy()
	global.Region = "us-east-1"

	c := &Clients{
		Region: cfg.Region,
		EC2:    ec2.NewFromConfig(cfg),
		CW:     cloudwatch.NewFromConfig(cfg),
		CE:     costexplorer.NewFromConfig(global),
	}
	c.Price = nat.LookupPrice(ctx, pricing.NewFromConfig(global), cfg.Region)
	return c, nil
}

// Watch runs one evaluation and returns the report text plus whether any gateway breached.
func Watch(ctx context.Context, c *Clients, o Options) (string, bool, error) {
	w := &nat.Watcher{EC2: c.EC2, CW: c.CW, Price: c.Price}
	samples, err := w.Watch(ctx, o.Window, o.Baseline)
	if err != nil {
		return "", false, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "spendfuse · NAT Gateway · %s · price %s\n", c.Region, c.Price)
	fmt.Fprintf(&b, "window %s, baseline %s, limits: $%.2f/h abs, %.1f× baseline\n\n", o.Window, o.Baseline, o.MaxUSDPerHour, o.MaxRatio)
	if len(samples) == 0 {
		b.WriteString("no available NAT gateways in this region\n")
		return b.String(), false, nil
	}
	fmt.Fprintf(&b, "%-22s %-18s %10s %12s %12s %7s  %s\n", "GATEWAY", "NAME", "GB/window", "BURN $/h", "BASE $/h", "RATIO", "STATUS")
	breached := false
	var alerts []string
	for _, s := range samples {
		v := nat.Evaluate(s, o.MaxUSDPerHour, o.MaxRatio)
		status := "ok"
		if v.Breach {
			status = "BREACH: " + v.Reason
			breached = true
			alerts = append(alerts, fmt.Sprintf("• %s (%s, %s): %s — %.2f GB in last %s", s.Gateway.ID, s.Gateway.Name, s.Gateway.VPC, v.Reason, s.GB, s.Window))
		}
		ratio := "—"
		if s.Ratio > 0 && !math.IsInf(s.Ratio, 0) {
			ratio = fmt.Sprintf("%.1f×", s.Ratio)
		}
		fmt.Fprintf(&b, "%-22s %-18s %10.3f %12.2f %12.2f %7s  %s\n", s.Gateway.ID, trunc(s.Gateway.Name, 18), s.GB, s.BurnPerHour, s.BaselinePerHr, ratio, status)
	}

	if breached {
		msg := fmt.Sprintf(":rotating_light: *spendfuse* NAT spend breach in %s\n%s\n_action: notify only (prototype)_", c.Region, strings.Join(alerts, "\n"))
		if o.DryRun {
			b.WriteString("\n[dry-run] would notify Slack:\n" + msg + "\n")
		} else if err := notify.Slack(ctx, o.SlackWebhook, msg); err != nil {
			fmt.Fprintf(&b, "\nslack notify failed: %v\n", err)
		} else if o.SlackWebhook != "" {
			b.WriteString("\nSlack notified.\n")
		}
	}
	return b.String(), breached, nil
}

// Calibrate compares CloudWatch-derived estimates with Cost Explorer actuals.
func Calibrate(ctx context.Context, c *Clients, o Options) (string, error) {
	w := &nat.Watcher{EC2: c.EC2, CW: c.CW, Price: c.Price}
	rs, err := w.Calibrate(ctx, c.CE, o.Days)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "spendfuse · NAT calibration · %s · price %s\n", c.Region, c.Price)
	b.WriteString("Compares CloudWatch bytes × price (our estimate) with Cost Explorer (what AWS billed), per UTC day.\n")
	b.WriteString("Note: Cost Explorer is region-agnostic here; if you run NAT in several regions, run per region and compare totals.\n\n")
	fmt.Fprintf(&b, "%-12s %10s %10s | %10s %10s | %10s %10s | %8s\n", "DAY", "EST GB", "BILLED GB", "EST DATA", "BILL DATA", "EST TOTAL", "BILL TOTAL", "ERROR")
	for _, r := range rs {
		errs := fmt.Sprintf("%+.1f%%", r.ErrorPct)
		if math.IsInf(r.ErrorPct, 1) {
			errs = "n/a"
		}
		fmt.Fprintf(&b, "%-12s %10.2f %10.2f | %10.2f %10.2f | %10.2f %10.2f | %8s\n",
			r.Day, r.EstGB, r.BilledGB, r.EstDataUSD, r.BilledDataUSD, r.EstTotalUSD, r.BilledTotalUSD, errs)
	}
	b.WriteString("\n" + nat.Summary(rs) + "\n")
	return b.String(), nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

var _ = aws.String // keep aws import for future options
