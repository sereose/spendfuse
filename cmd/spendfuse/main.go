// spendfuse: a fuse for your cloud bill. Prototype signals: NAT Gateway, Logs Insights.
//
//	spendfuse nat  watch      --region eu-central-1 --max-usd-hour 20 --max-ratio 3
//	spendfuse nat  calibrate  --region eu-central-1 --days 3
//	spendfuse logs watch      --region eu-central-1 --window 1h --max-projected-monthly 500
//	spendfuse logs calibrate  --region eu-central-1 --days 2
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sereose/spendfuse/internal/run"
)

var version = "dev"

func main() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(2)
	}
	signal, sub := os.Args[1], os.Args[2]
	fs := flag.NewFlagSet("spendfuse "+signal+" "+sub, flag.ExitOnError)

	o := run.Options{}
	lo := run.LogsOptions{}
	fs.StringVar(&o.Region, "region", os.Getenv("AWS_REGION"), "AWS region (default $AWS_REGION / profile)")
	fs.StringVar(&o.Profile, "profile", os.Getenv("AWS_PROFILE"), "AWS shared config profile")
	fs.StringVar(&o.SlackWebhook, "slack-webhook", os.Getenv("SLACK_WEBHOOK"), "Slack incoming webhook URL")
	fs.BoolVar(&o.DryRun, "dry-run", true, "print what would be sent instead of notifying")
	fs.IntVar(&o.Days, "days", 3, "calibrate: number of complete UTC days to compare")
	// NAT
	fs.DurationVar(&o.Window, "window", 0, "evaluation window (default: nat 5m, logs 1h)")
	fs.DurationVar(&o.Baseline, "baseline", 24*time.Hour, "nat: baseline period for ratio")
	fs.Float64Var(&o.MaxUSDPerHour, "max-usd-hour", 0, "absolute burn limit $/h (default: nat 20 per gateway, logs 5 total; 0 = off)")
	fs.Float64Var(&o.MaxRatio, "max-ratio", 3, "nat: breach when burn exceeds this multiple of baseline (0 = off)")
	// Logs Insights
	fs.Float64Var(&lo.MaxProjectedMonthly, "max-projected-monthly", 500, "logs: breach when one repeating query projects above this $/month (0 = off)")
	fs.IntVar(&lo.TopN, "top", 15, "logs: patterns to print")
	showVersion := fs.Bool("version", false, "print version")
	_ = fs.Parse(os.Args[3:])
	if *showVersion {
		fmt.Println("spendfuse", version)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := run.Connect(ctx, o)
	if err != nil {
		fail(err)
	}

	switch signal + " " + sub {
	case "nat watch":
		if o.Window == 0 {
			o.Window = 5 * time.Minute
		}
		if o.MaxUSDPerHour == 0 && !isSet(fs, "max-usd-hour") {
			o.MaxUSDPerHour = 20
		}
		out, breached, err := run.Watch(ctx, c, o)
		finish(out, breached, err)
	case "nat calibrate":
		out, err := run.Calibrate(ctx, c, o)
		finish(out, false, err)
	case "logs watch":
		lo.Window, lo.SlackWebhook, lo.DryRun = o.Window, o.SlackWebhook, o.DryRun
		if lo.Window == 0 {
			lo.Window = time.Hour
		}
		lo.MaxUSDPerHour = o.MaxUSDPerHour
		if lo.MaxUSDPerHour == 0 && !isSet(fs, "max-usd-hour") {
			lo.MaxUSDPerHour = 5
		}
		out, breached, err := run.LogsWatch(ctx, c, lo)
		finish(out, breached, err)
	case "logs calibrate":
		lo.Days = o.Days
		out, err := run.LogsCalibrate(ctx, c, lo)
		finish(out, false, err)
	default:
		usage()
		os.Exit(2)
	}
}

func isSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func finish(out string, breached bool, err error) {
	if err != nil {
		fail(err)
	}
	fmt.Print(out)
	if breached {
		os.Exit(3)
	}
	os.Exit(0)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  spendfuse nat  watch     [--region R] [--window 5m] [--max-usd-hour 20] [--max-ratio 3] [--slack-webhook URL] [--dry-run=false]
  spendfuse nat  calibrate [--region R] [--days 3]
  spendfuse logs watch     [--region R] [--window 1h] [--max-usd-hour 5] [--max-projected-monthly 500] [--top 15]
  spendfuse logs calibrate [--region R] [--days 2]

exit codes: 0 ok · 3 breach detected · 1 error`)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "spendfuse:", err)
	os.Exit(1)
}
