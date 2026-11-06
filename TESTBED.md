# Testbed: from a fresh AWS account to a calibration number

Goal: measure how closely spendfuse's minute-level estimates match the AWS bill, using an account you control. Budget: **~$3/day for 3–4 days**, then delete everything.

## 0. Account (one-time, ~20 min)

1. Create the account at aws.amazon.com. Use a dedicated email; a card is required. New accounts get Free Tier credits — NAT is not free, but credits may cover the test.
2. Sign in as root once, enable MFA, then **stop using root**.
3. Billing console → **Cost Explorer → Enable**. Free; data appears within 24h. Without this, `calibrate` cannot read actuals.
4. Billing console → Billing preferences → enable **"Receive Free Tier usage alerts"** and set a **Budget** at e.g. $20 (as a safety net for the test itself).
5. Create an admin identity for the CLI. Simplest: IAM Identity Center (SSO) with `AdministratorAccess`; or an IAM user with that policy and an access key. Configure the CLI:
   ```bash
   aws configure sso    # or: aws configure
   aws sts get-caller-identity
   ```
6. Pick one region for everything (examples use `eu-central-1`) and `export AWS_REGION=eu-central-1`.

## 1. Deploy the testbed (~5 min)

```bash
aws cloudformation deploy --stack-name spendfuse-testbed \
  --template-file deploy/testbed.yaml --capabilities CAPABILITY_IAM
```

What it creates:

| Resource | Purpose | Cost |
|---|---|---|
| VPC, public + private subnet, **NAT Gateway** | the thing we measure | ~$1.1/day + $0.045–0.06/GB processed |
| `t4g.nano` in the private subnet, cron every 15 min | pulls 300 MB through the NAT (≈29 GB/day) | ~$0.10/day; data transfer in is free |
| Log group `/spendfuse/testbed/noise` + Lambda every 5 min | writes ~200 KB of logs, then runs the **same 7-day Logs Insights query** — an auto-refreshing dashboard widget | cents; scanning crosses the 5 GB free tier on day 2–3 |

Expected NAT processing per day: `DownloadMB × (1440 / IntervalMinutes) / 1000` GB → defaults: 300 × 96 / 1000 ≈ **29 GB ≈ $1.5/day** in eu-central-1.

Check it's working after ~10 minutes:

```bash
aws cloudwatch get-metric-statistics --namespace AWS/NATGateway --metric-name BytesInFromDestination \
  --dimensions Name=NatGatewayId,Value=$(aws cloudformation describe-stacks --stack-name spendfuse-testbed \
  --query 'Stacks[0].Outputs[?OutputKey==`NatGatewayId`].OutputValue' --output text) \
  --start-time $(date -u -v-30M +%FT%TZ 2>/dev/null || date -u -d '30 min ago' +%FT%TZ) \
  --end-time $(date -u +%FT%TZ) --period 300 --statistics Sum --query 'Datapoints[].Sum'
```

Non-zero numbers mean the puller is pulling.

## 2. Watch it live (day 0)

```bash
./spendfuse nat  watch --region $AWS_REGION --window 15m --max-usd-hour 0.05
./spendfuse logs watch --region $AWS_REGION --window 1h
```

The NAT line should show ~0.3 GB per 15-minute window. The Logs line should show one pattern with `REFRESH`, 12 runs/hour, projection growing day by day. Lower `--max-usd-hour` until it breaches to see the Slack message (dry-run).

Optionally deploy the scheduled watcher too (`deploy/cloudformation.yaml`, see README) so there is a 5-minute history in CloudWatch Logs to compare against later.

## 3. Wait

Cost Explorer needs the day to close and then ~24h to settle. Timeline with a deploy on day 0:

| Day | What's available |
|---|---|
| 1 | nothing reliable yet |
| 2 | day 1 billed; `--days 1` works |
| 3 | days 1–2 billed |
| 4 | days 1–3 billed — **enough**; delete the stack after calibrating |

## 4. Calibrate (day 3 or 4)

```bash
./spendfuse nat  calibrate --region $AWS_REGION --days 3
./spendfuse logs calibrate --region $AWS_REGION --days 3
```

Read the last line of each. What the result means:

| Error | Interpretation | Next step |
|---|---|---|
| **< 5%** | estimate is bill-grade; thresholds can be tight | build actions |
| **5–10%** | good; thresholds with 10–15% margin | build actions |
| **10–25%** | usable for ratio-based alerts, not for tight $ limits | find the systematic cause (see README table) before actions |
| **> 25%** | hypothesis fails for this signal | stop, investigate: region mismatch, GiB/GB, free tier, missing usage types |

Known effects to expect:
- **Free tier**: the first 5 GB scanned/month and some NAT hours may be billed at $0. `calibrate` falls back to comparing **GB quantities** when billed USD is zero and says so.
- **Partial first day**: day 0 is partial by definition; that's why `calibrate` starts from two days ago.
- **Hours**: the bill has 24 gateway-hours/day; the estimate uses the same, so the hourly part should match exactly — the data part is what we're really testing.

## 5. Tear down

```bash
aws cloudformation delete-stack --stack-name spendfuse-testbed
aws cloudformation wait stack-delete-complete --stack-name spendfuse-testbed
```

The NAT gateway and EIP are the only things that keep billing if forgotten; confirm in the console that both are gone. The log group is deleted with the stack.

## What to record

For the spec and the eventual landing page, keep: region, days, GB/day pulled, estimated vs billed per day, final error %, and anything surprising. That table is the whole result of this exercise.
