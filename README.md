# spendfuse — prototype: NAT Gateway spend signal

A fuse for your cloud bill. This prototype answers one question before anything else gets built:

> **How closely does a cost estimate derived from CloudWatch byte counters (available within minutes) match what AWS actually bills for NAT Gateway (available ~24h later)?**

If the answer is "within 10%", near-real-time spend guardrails are feasible. If it's "off by 50%", the whole idea needs rethinking. Everything else — more signals, actions, undo — waits for that number.

## What it does

- `spendfuse nat watch` — for every NAT gateway in the region: bytes processed in the last 5 minutes × regional price (from the Pricing API, with fallback) → **burn rate in $/h now**, compared with the 24h baseline. Breaches an absolute limit or a ratio → Slack message (dry-run by default). **Notify only; nothing is touched.**
- `spendfuse nat calibrate` — for the last N complete UTC days: our estimate vs Cost Explorer's actual NAT charges, per day, with the error in %.
- Lambda + CloudFormation to run `watch` every 5 minutes and keep the history in CloudWatch Logs.

IAM needed: `ec2:DescribeNatGateways`, `cloudwatch:GetMetricData`, `pricing:GetProducts`, and for calibrate `ce:GetCostAndUsage`. All read-only.

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

## Status

Prototype. No actions, single signal, single region per run. Unit tests cover the math and the Cost Explorer parsing with fakes; the number that matters comes from running `calibrate` against a real account.

MIT.
