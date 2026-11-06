# spendfuse — prototype: spend signals that fire in minutes, not days

A fuse for your cloud bill. AWS billing data lags 8–24 hours, so Budgets, Cost Anomaly Detection and every dashboard built on them tell you about a leak after it has run all night. Usage metrics and API history are available within minutes. spendfuse estimates **burn rate now** from those and compares it against what AWS bills later, to prove (or disprove) that near-real-time guardrails are possible.

Two signals so far:

| Signal | Source (minutes) | Why native tools miss it |
|---|---|---|
| **NAT Gateway** | CloudWatch `BytesInFromSource` + `BytesInFromDestination` × regional $/GB | Budgets/Anomaly Detection see it a day later; no per-gateway view |
| **Logs Insights** | `DescribeQueries` → `BytesScanned` per query × $/GB scanned | **No CloudWatch metric exists** for bytes scanned, so no alarm can be set; a dashboard widget re-running a wide query every minute can cost hundreds of $/month unnoticed |

The prototype answers one question first: **how closely does the estimate match the bill?** If it's within ~10%, guardrails with actions are feasible. Everything else — more signals, actions, undo — waits for that number.

## What it does

- `spendfuse nat watch` — per gateway: bytes in the last 5 minutes × price → **$/h now** vs the 24h baseline. Breach on an absolute limit or a ratio → Slack (dry-run by default).
- `spendfuse logs watch` — groups the last hour's Logs Insights queries by (normalised query text, log group), prices each pattern, detects **auto-refresh** (≥5 runs, ≤15 min apart) and projects **$/month if it continues**. Breach on total $/h or on one pattern's projection.
- `spendfuse nat|logs calibrate` — for the last N complete UTC days: our estimate vs Cost Explorer actuals, per day, with error in %.
- Lambda + CloudFormation to run both every 5 minutes; history stays in CloudWatch Logs.

**Notify only; nothing is touched.** IAM: `ec2:DescribeNatGateways`, `cloudwatch:GetMetricData`, `logs:DescribeQueries`, `pricing:GetProducts`, plus `ce:GetCostAndUsage` for calibrate. All read-only.

## Run it

Requires AWS credentials in the environment or a profile.

```bash
make build
./bin/spendfuse nat calibrate --region eu-central-1 --days 3
```

Example output:

```
spendfuse · NAT calibration · eu-central-1 · price $0.052/h + $0.052/GB (eu-central-1, pricing-api)
DAY            EST GB  BILLED GB |   EST DATA  BILL DATA |  EST TOTAL BILL TOTAL |    ERROR
2026-11-01      84.21      84.50 |       4.38       4.39 |       6.88       6.89 |    -0.2%
...
3 day(s): estimated $20.41 vs billed $20.67 → error -1.3% — excellent (<5%)
```

Then the live signal:

```bash
./bin/spendfuse nat watch --region eu-central-1 --max-usd-hour 20 --max-ratio 3
```

Exit code 3 means at least one gateway breached. Add `--dry-run=false --slack-webhook https://hooks.slack.com/...` to actually post.

Logs Insights:

```bash
./bin/spendfuse logs watch --region eu-central-1 --window 1h --max-projected-monthly 500
```

```
spendfuse · Logs Insights · eu-central-1 · $0.0063/GB scanned (pricing-api)
window 1h0m0s: 61 queries, 3002.0 GB scanned, $18.91 → burn $18.91/h · limits: $5.00/h, $500/month per pattern

RUNS   INTERVAL   GB              USD     FLAG    PROJ $/mo  LOG GROUP                        QUERY
60     1m0s          3000.0     18.90  REFRESH        13608  /aws/lambda/api                  fields @timestamp, @message | filter @message like /ERROR/
1      —                2.0      0.01                     9  /aws/ecs/web                     stats count() by bin(5m)

BREACH:
  • Logs Insights burn $18.91/h exceeds $5.00/h
  • auto-refreshing query (dashboard or open tab) on /aws/lambda/api: 60 runs every 1m0s, 3000.0 GB, → ~$13608/month
```

```bash
./bin/spendfuse logs calibrate --region eu-central-1 --days 2
```

Note: `DescribeQueries` keeps a bounded history (~1000 most recent queries per region). On a busy account only the last day or two are complete; the report flags truncation. Cost Explorer is account-wide while queries are per region — run per region and compare totals.

### Deploy the scheduled watcher

```bash
make lambda
aws s3 cp dist/spendfuse-lambda.zip s3://YOUR-BUCKET/
aws cloudformation deploy --stack-name spendfuse-nat --template-file deploy/cloudformation.yaml \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides CodeBucket=YOUR-BUCKET SlackWebhook=https://hooks.slack.com/... DryRun=true
```

Runs every 5 minutes, ~$0.20/month in Lambda + GetMetricData.

## How the estimate works

- Billable bytes = `BytesInFromSource + BytesInFromDestination` (everything entering the gateway from either side). The `BytesOut*` metrics mirror these and would double-count.
- Price = regional NAT on-demand: `$/GB processed` + `$/gateway-hour`. Both come from the Pricing API (`productFamily=NAT Gateway`, `regionCode=<region>`); if that fails, a built-in table is used and the report says `fallback`.
- The watch window ends 2 minutes before now, because NAT metrics land in CloudWatch with a 1–3 minute delay.
- **Not included:** Data Transfer Out to the internet (billed by EC2, not NAT, ~$0.09/GB). A NAT egress spike usually implies a DTO spike too, so the true cost of a breach is higher than the number shown. A later signal covers DTO separately.

## Things that could make the calibration disagree (what to look for)

| Symptom | Likely cause |
|---|---|
| Estimate consistently ~7% high | GiB (CloudWatch) vs GB (billing) — AWS bills per GB = 1024³ bytes for NAT, so this should *not* appear; if it does, the price-list unit differs |
| Billed GB > estimated GB | A gateway deleted during the period (no longer listed) still billed; or NAT in another region (Cost Explorer is account-wide, CloudWatch is per region) |
| Billed hours ≠ 24 × gateways | Gateways created/deleted mid-day; fine |
| Estimate 0, bill > 0 | Metrics not available (very new gateway) or wrong region |
| Large negative error | Private NAT gateways or traffic through VPC endpoints — check which gateways are actually in use |

## Signal backlog (from real 2026 bill stories)

| Leak | Fast source | Planned action |
|---|---|---|
| S3 request/egress flood on a public bucket ("$15k S3 bill for DDoS") | S3 request metrics (`AllRequests`, `BytesDownloaded`, 1-min, per bucket) | notify → enable Block Public Access on that bucket (unauthenticated 4xx requests are not billed since 2024) → bucket policy deny |
| Data Transfer Out spike | `NetworkOut` on EC2/ALB, CloudFront `BytesDownloaded` | notify → WAF rate rule → deny |
| Bedrock token burn | `InputTokenCount`/`OutputTokenCount` per model | notify → IAM deny `bedrock:InvokeModel` for the role |
| EC2 launch storm / leaked key | CloudTrail `RunInstances` via EventBridge | notify → deny `ec2:RunInstances` → terminate instances launched after the breach |
| Lambda runaway | `Invocations` × `Duration`, `ConcurrentExecutions` | notify → reserved concurrency 0 |
| CloudWatch Logs ingestion | `IncomingBytes` per log group | notify → retention/subscription change |

Out of scope: third-party SaaS bills (New Relic, Datadog ingest overages). Same pattern, but each vendor needs its own usage API and the kill switch lives on their side — a separate product if ever.

## For testers

See [TESTERS.md](TESTERS.md): what the tool reads, the exact IAM policy, and the two commands that produce the calibration number.

## Status

Prototype. No actions, two signals, single region per run. Unit tests cover the math and the Cost Explorer parsing with fakes; the number that matters comes from running `calibrate` against a real account.

MIT.
