// Package logsinsights implements the CloudWatch Logs Insights spend signal.
//
// Logs Insights bills per GB *scanned*. There is no CloudWatch metric for
// bytes scanned, so neither alarms nor Budgets can catch a dashboard that
// re-runs a wide query on every refresh. DescribeQueries, however, returns
// every recent query with its BytesScanned — enough to compute burn rate,
// find the repeating query, and project what it costs per month.
package logsinsights

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

const gib = 1024 * 1024 * 1024

// LogsAPI is the client subset we need.
type LogsAPI interface {
	DescribeQueries(ctx context.Context, in *cloudwatchlogs.DescribeQueriesInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeQueriesOutput, error)
}

// Query is one executed Logs Insights query.
type Query struct {
	ID           string
	Text         string
	LogGroup     string
	CreatedAt    time.Time
	BytesScanned int64
	Status       string
}

// Pattern groups repeated runs of the same query against the same log group.
type Pattern struct {
	Hash         string
	Text         string
	LogGroup     string
	Runs         int
	Bytes        int64
	USD          float64
	FirstSeen    time.Time
	LastSeen     time.Time
	AvgInterval  time.Duration // between runs; 0 if Runs < 2
	ProjectedUSD float64       // per 30 days if this cadence continues
	AutoRefresh  bool          // Runs ≥ 5 and AvgInterval ≤ 15m
}

// Report is the result of one evaluation.
type Report struct {
	Window      time.Duration
	Queries     int
	Bytes       int64
	USD         float64
	BurnPerHour float64
	Patterns    []Pattern // sorted by USD desc
	Truncated   bool      // DescribeQueries returned its cap; older queries may be missing
}

// Watcher evaluates Logs Insights spend.
type Watcher struct {
	Logs       LogsAPI
	PricePerGB float64
	Now        func() time.Time
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Recent lists queries created at or after `since`. DescribeQueries returns
// the most recent ~1000 queries in the region; on a very busy account the
// window may be cut short, which Report.Truncated flags.
func (w *Watcher) Recent(ctx context.Context, since time.Time) ([]Query, bool, error) {
	var out []Query
	var token *string
	total := 0
	for {
		res, err := w.Logs.DescribeQueries(ctx, &cloudwatchlogs.DescribeQueriesInput{
			MaxResults: aws.Int32(1000),
			NextToken:  token,
		})
		if err != nil {
			return nil, false, fmt.Errorf("describe queries: %w", err)
		}
		for _, q := range res.Queries {
			total++
			created := time.UnixMilli(aws.ToInt64(q.CreateTime))
			if created.Before(since) {
				continue
			}
			out = append(out, Query{
				ID:           aws.ToString(q.QueryId),
				Text:         aws.ToString(q.QueryString),
				LogGroup:     aws.ToString(q.LogGroupName),
				CreatedAt:    created,
				BytesScanned: int64(aws.ToFloat64(q.BytesScanned)),
				Status:       string(q.Status),
			})
		}
		if res.NextToken == nil {
			break
		}
		token = res.NextToken
	}
	// If the oldest query we saw is still inside the window, there may be more
	// we never got: the API only keeps a bounded history.
	truncated := false
	if len(out) > 0 && total >= 1000 {
		oldest := out[0].CreatedAt
		for _, q := range out {
			if q.CreatedAt.Before(oldest) {
				oldest = q.CreatedAt
			}
		}
		truncated = oldest.After(since.Add(5 * time.Minute))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, truncated, nil
}

// Watch evaluates the last `window`.
func (w *Watcher) Watch(ctx context.Context, window time.Duration) (*Report, error) {
	since := w.now().Add(-window)
	qs, truncated, err := w.Recent(ctx, since)
	if err != nil {
		return nil, err
	}
	r := Analyze(qs, w.PricePerGB, window)
	r.Truncated = truncated
	return r, nil
}

// Analyze is the pure part: groups queries into patterns and prices them.
func Analyze(qs []Query, pricePerGB float64, window time.Duration) *Report {
	r := &Report{Window: window, Queries: len(qs)}
	groups := map[string]*Pattern{}
	for _, q := range qs {
		r.Bytes += q.BytesScanned
		h := hashQuery(q.Text, q.LogGroup)
		p, ok := groups[h]
		if !ok {
			p = &Pattern{Hash: h, Text: q.Text, LogGroup: q.LogGroup, FirstSeen: q.CreatedAt, LastSeen: q.CreatedAt}
			groups[h] = p
		}
		p.Runs++
		p.Bytes += q.BytesScanned
		if q.CreatedAt.Before(p.FirstSeen) {
			p.FirstSeen = q.CreatedAt
		}
		if q.CreatedAt.After(p.LastSeen) {
			p.LastSeen = q.CreatedAt
		}
	}
	r.USD = float64(r.Bytes) / gib * pricePerGB
	if window > 0 {
		r.BurnPerHour = r.USD / window.Hours()
	}
	for _, p := range groups {
		p.USD = float64(p.Bytes) / gib * pricePerGB
		if p.Runs >= 2 {
			span := p.LastSeen.Sub(p.FirstSeen)
			p.AvgInterval = span / time.Duration(p.Runs-1)
			if p.AvgInterval > 0 {
				perRun := p.USD / float64(p.Runs)
				runsPerMonth := float64(30*24*time.Hour) / float64(p.AvgInterval)
				p.ProjectedUSD = perRun * runsPerMonth
			}
			p.AutoRefresh = p.Runs >= 5 && p.AvgInterval <= 15*time.Minute
		} else if window > 0 {
			// Single run: project as if it ran once per window.
			p.ProjectedUSD = p.USD * float64(30*24*time.Hour) / float64(window)
		}
		r.Patterns = append(r.Patterns, *p)
	}
	sort.Slice(r.Patterns, func(i, j int) bool { return r.Patterns[i].USD > r.Patterns[j].USD })
	return r
}

var ws = regexp.MustCompile(`\s+`)

// hashQuery normalises whitespace so the same dashboard widget hashes the same
// even if the console reformats it.
func hashQuery(text, logGroup string) string {
	norm := strings.ToLower(ws.ReplaceAllString(strings.TrimSpace(text), " "))
	sum := sha1.Sum([]byte(norm + "\n" + logGroup))
	return hex.EncodeToString(sum[:])[:10]
}

// Verdict is the policy decision for a report.
type Verdict struct {
	Breach  bool
	Reasons []string
}

// Evaluate applies: absolute burn $/h, and per-pattern projected $/month.
func Evaluate(r *Report, maxUSDPerHour, maxProjectedMonthly float64) Verdict {
	var v Verdict
	if maxUSDPerHour > 0 && r.BurnPerHour > maxUSDPerHour {
		v.Breach = true
		v.Reasons = append(v.Reasons, fmt.Sprintf("Logs Insights burn $%.2f/h exceeds $%.2f/h", r.BurnPerHour, maxUSDPerHour))
	}
	for _, p := range r.Patterns {
		if maxProjectedMonthly > 0 && p.ProjectedUSD > maxProjectedMonthly && p.Runs >= 2 {
			v.Breach = true
			kind := "repeating query"
			if p.AutoRefresh {
				kind = "auto-refreshing query (dashboard or open tab)"
			}
			v.Reasons = append(v.Reasons, fmt.Sprintf("%s on %s: %d runs every %s, %.1f GB, → ~$%.0f/month",
				kind, p.LogGroup, p.Runs, p.AvgInterval.Round(time.Second), float64(p.Bytes)/gib, p.ProjectedUSD))
		}
	}
	return v
}

// Short trims a query for display.
func Short(text string, n int) string {
	t := ws.ReplaceAllString(strings.TrimSpace(text), " ")
	if len(t) <= n {
		return t
	}
	return t[:n-1] + "…"
}

var _ cwltypes.QueryStatus // keep types import explicit for readers
