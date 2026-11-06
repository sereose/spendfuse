# Help calibrate spendfuse — 30 seconds, read-only

**What I'm testing:** whether cloud spend can be estimated from usage metrics *within minutes* accurately enough to act on, instead of waiting ~24h for AWS billing. The prototype compares its own estimate with what Cost Explorer actually billed and prints the error in %. I need that number from real accounts, not from my sandbox.

**What you'd run:** one static binary, read-only, no agent, no account signup, nothing leaves your machine except the terminal output you choose to send me.

## What it reads — and what it does not

| Reads (read-only API calls) | Does **not** read |
|---|---|
| List of NAT gateways (`ec2:DescribeNatGateways`) | Any log *contents* |
| NAT byte counters from CloudWatch (`cloudwatch:GetMetricData`) | Secrets, env vars, S3 objects, databases |
| Recent Logs Insights queries: text, log group, bytes scanned (`logs:DescribeQueries`) | Who ran a query (no CloudTrail access) |
| Public price list (`pricing:GetProducts`) | Anything in other AWS accounts |
| Daily NAT / Logs Insights cost totals (`ce:GetCostAndUsage`) | — |

It makes **no write calls**. The IAM policy below is the complete permission set.

Note on privacy: the Logs Insights output includes **query text and log group names**. Redact anything sensitive before sending, or just send the summary line.

## Run it

1. Download the binary for your platform from [Releases](https://github.com/sereose/spendfuse/releases) (macOS arm64/amd64, Linux arm64/amd64), or build from source with Go 1.26+: `go install github.com/sereose/spendfuse/cmd/spendfuse@latest`.

   macOS example:
   ```bash
   curl -sSL https://github.com/sereose/spendfuse/releases/latest/download/spendfuse_$(curl -sSL https://api.github.com/repos/sereose/spendfuse/releases/latest | grep -o '"tag_name": "v[^"]*' | cut -d'v' -f2)_darwin_arm64.tar.gz | tar xz spendfuse
   xattr -d com.apple.quarantine ./spendfuse 2>/dev/null; chmod +x ./spendfuse
   ```

2. With your usual AWS credentials (`AWS_PROFILE`, SSO session, or env vars) and a region where you have NAT gateways and/or use Logs Insights:

   ```bash
   ./spendfuse nat calibrate  --region eu-central-1 --days 3
   ```
   ```bash
   ./spendfuse logs calibrate --region eu-central-1 --days 2
   ```

   Optional, the live view (also harmless):
   ```bash
   ./spendfuse logs watch --region eu-central-1 --window 24h --top 10
   ```

3. Send me the output (email, Slack, DM — whatever is easy). The lines that matter most are the ones starting with `N day(s): estimated $X vs billed $Y → error ±Z%`.

If Cost Explorer has never been opened in the account, `ce:GetCostAndUsage` fails with a message asking to enable it (Billing console → Cost Explorer). Enabling it is free; data appears within 24h.

## Minimal IAM policy

If you'd rather not use your admin credentials, create a user or role with exactly this:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "ec2:DescribeNatGateways",
      "cloudwatch:GetMetricData",
      "logs:DescribeQueries",
      "pricing:GetProducts",
      "ce:GetCostAndUsage"
    ],
    "Resource": "*"
  }]
}
```

## What I'll do with it

- Aggregate the error percentages across accounts to decide whether the approach is viable (<10% error) or not.
- Nothing else. I won't publish your numbers, log group names or account details. If you'd like, I'll send back a short note on what the output says about your account (a dashboard that scans more than it should, a NAT gateway that costs more than expected).

## Three questions, if you have two more minutes

1. Have you ever had an AWS bill surprise? What was it, and how did you find out?
2. What catches cost spikes for you today — Budgets, Anomaly Detection, a vendor tool, a script, nothing?
3. If a tool could *stop* a runaway resource (not just alert) within 15 minutes, would you let it — and under what conditions?

Thank you. Source: [github.com/sereose/spendfuse](https://github.com/sereose/spendfuse) — read it before running if you like; it's small.

---

### Коротко по-русски

Один бинарник, только чтение, пять IAM-разрешений (список выше). Сравнивает оценку расходов по метрикам CloudWatch с тем, что реально выставил Cost Explorer, и печатает погрешность в процентах. Логи не читает — только названия лог-групп и текст запросов Logs Insights (можно вырезать перед отправкой). Запуск:

```bash
./spendfuse nat calibrate --region <регион> --days 3
./spendfuse logs calibrate --region <регион> --days 2
```

Мне нужна строка `error ±Z%` и, если есть две минуты, ответы на три вопроса выше.
