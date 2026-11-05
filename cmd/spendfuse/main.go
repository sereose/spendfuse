// spendfuse: a fuse for your cloud bill. Prototype: NAT Gateway signal.
//
//	spendfuse nat watch      --region eu-central-1 --max-usd-hour 20 --max-ratio 3
//	spendfuse nat calibrate  --region eu-central-1 --days 3
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
	if len(os.Args) < 3 || os.Args[1] != "nat" {
		usage()
		os.Exit(2)
	}
	sub := os.Args[2]
	fs := flag.NewFlagSet("spendfuse nat "+sub, flag.ExitOnError)
	o := run.Options{}
	fs.StringVar(&o.Region, "region", os.Getenv("AWS_REGION"), "AWS region (default $AWS_REGION / profile)")
	fs.StringVar(&o.Profile, "profile", os.Getenv("AWS_PROFILE"), "AWS shared config profile")
	fs.DurationVar(&o.Window, "window", 5*time.Minute, "evaluation window")
	fs.DurationVar(&o.Baseline, "baseline", 24*time.Hour, "baseline period for ratio")
	fs.Float64Var(&o.MaxUSDPerHour, "max-usd-hour", 20, "absolute burn limit, $/h per gateway (0 = off)")
	fs.Float64Var(&o.MaxRatio, "max-ratio", 3, "breach when burn exceeds this multiple of baseline (0 = off)")
	fs.StringVar(&o.SlackWebhook, "slack-webhook", os.Getenv("SLACK_WEBHOOK"), "Slack incoming webhook URL")
	fs.BoolVar(&o.DryRun, "dry-run", true, "print what would be sent instead of notifying")
	fs.IntVar(&o.Days, "days", 3, "calibrate: number of complete UTC days to compare")
	showVersion := fs.Bool("version", false, "print version")
	_ = fs.Parse(os.Args[3:])
	if *showVersion {
		fmt.Println("spendfuse", version)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := run.Connect(ctx, o)
	if err != nil {
		fail(err)
	}
	switch sub {
	case "watch":
		out, breached, err := run.Watch(ctx, c, o)
		if err != nil {
			fail(err)
		}
		fmt.Print(out)
		if breached {
			os.Exit(3)
		}
	case "calibrate":
		out, err := run.Calibrate(ctx, c, o)
		if err != nil {
			fail(err)
		}
		fmt.Print(out)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  spendfuse nat watch     [--region R] [--window 5m] [--max-usd-hour 20] [--max-ratio 3] [--slack-webhook URL] [--dry-run=false]
  spendfuse nat calibrate [--region R] [--days 3]

exit codes: 0 ok · 3 breach detected · 1 error`)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "spendfuse:", err)
	os.Exit(1)
}
