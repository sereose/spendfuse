// Lambda entry point: runs the NAT watch on a schedule (EventBridge rate(5 minutes)).
// Configuration via environment variables set by the CloudFormation template.
package main

import (
	"context"
	"log"
	"os"
	"strconv"
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
	c, err := run.Connect(ctx, o)
	if err != nil {
		return "", err
	}
	out, breached, err := run.Watch(ctx, c, o)
	if err != nil {
		return "", err
	}
	log.Print(out) // CloudWatch Logs keeps the history for calibration review
	if breached {
		log.Print("BREACH")
	}
	return out, nil
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
