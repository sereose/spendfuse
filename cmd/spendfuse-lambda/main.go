// Lambda entry point: runs the NAT watch on a schedule (EventBridge rate(5 minutes)).
// Configuration via environment variables set by the CloudFormation template.
package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/sereose/spendfuse/internal/run"
)

func handler(ctx context.Context) (string, error) {
	o := run.Options{
		Region:        os.Getenv("AWS_REGION"),
		Window:        envDuration("SPENDFUSE_WINDOW", 5*time.Minute),
		Baseline:      envDuration("SPENDFUSE_BASELINE", 24*time.Hour),
		MaxUSDPerHour: envFloat("SPENDFUSE_MAX_USD_HOUR", 20),
		MaxRatio:      envFloat("SPENDFUSE_MAX_RATIO", 3),
		SlackWebhook:  os.Getenv("SLACK_WEBHOOK"),
		DryRun:        os.Getenv("SPENDFUSE_DRY_RUN") == "true",
	}
	lo := run.LogsOptions{
		Window:              envDuration("SPENDFUSE_LOGS_WINDOW", time.Hour),
		MaxUSDPerHour:       envFloat("SPENDFUSE_LOGS_MAX_USD_HOUR", 5),
		MaxProjectedMonthly: envFloat("SPENDFUSE_LOGS_MAX_PROJECTED_MONTHLY", 500),
		SlackWebhook:        o.SlackWebhook,
		DryRun:              o.DryRun,
		TopN:                10,
	}
	c, err := run.Connect(ctx, o)
	if err != nil {
		return "", err
	}
	var all []string
	anyBreach := false
	for _, s := range []struct {
		name string
		fn   func() (string, bool, error)
	}{
		{"nat", func() (string, bool, error) { return run.Watch(ctx, c, o) }},
		{"logs", func() (string, bool, error) { return run.LogsWatch(ctx, c, lo) }},
	} {
		if os.Getenv("SPENDFUSE_SIGNALS") != "" && !strings.Contains(os.Getenv("SPENDFUSE_SIGNALS"), s.name) {
			continue
		}
		out, breached, err := s.fn()
		if err != nil {
			log.Printf("%s: %v", s.name, err) // one failing signal must not silence the others
			continue
		}
		log.Print(out) // CloudWatch Logs keeps the history for calibration review
		all = append(all, out)
		anyBreach = anyBreach || breached
	}
	if anyBreach {
		log.Print("BREACH")
	}
	return strings.Join(all, "\n"), nil
}

func envDuration(k string, d time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return d
}

func envFloat(k string, d float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return d
}

func main() { lambda.Start(handler) }
