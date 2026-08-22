# Vasuli — Demo Guide

This guide walks through the complete demo sequence. Run it twice in rehearsal before the submission date.

---

## Prerequisites

All services running. Start in this order and wait for each to be ready before starting the next.

```bash
# Terminal 1 — Inference-Python
cd vasuli/var-thon/services/inference-py
uv run python main.py
# Wait for: "Inference Engine running on port :50051"

# Terminal 2 — var-thon Orchestrator-Go
cd vasuli/var-thon/services/orchestrator-go
go run ./cmd/gateway-server \
  -profile recovery_agent \
  -port :50052 \
  -inference localhost:50051 \
  -recovery http://localhost:8090
# Wait for: "Gateway server listening on :50052"

# Terminal 3 — Recovery Orchestrator
cd vasuli/recovery-orchestrator
go run ./cmd/main.go \
  -port :8090 \
  -db ./vasuli.db \
  -razorpay-key-id rzp_test_XXXX \
  -razorpay-key-secret YYYY \
  -razorpay-webhook-secret ZZZZ
# Wait for: "Recovery Orchestrator listening on :8090"

# Terminal 4 — AetherRTC
cd aetherRTC
go run ./cmd/gateway/main.go
# Wait for: "Listening on ws://localhost:8080/ws"
```

**Warm the LLM before the demo** (eliminates cold-start latency):

```bash
curl -s -X POST http://localhost:11434/api/generate \
  -d '{"model":"qwen2.5:3b","prompt":"hi","stream":false}' > /dev/null
```

---

## Step 1: Load the Recovery Campaign

```bash
curl -s -X POST http://localhost:8090/api/v1/campaigns \
  -H "Content-Type: application/json" \
  -d @recovery-orchestrator/testdata/sample_campaign.json | python3 -m json.tool
```

Expected output:

```json
{
  "campaign_id": "aug-emi-2026",
  "name": "August Failed EMI Recovery",
  "total": 20,
  "status": "active",
  "created_at": "2026-09-01T10:00:00Z"
}
```

**What to say to judges:** "We have 20 failed payment accounts queued. Each has a customer name, outstanding amount, product, and due date. The system prompt for each call is pre-rendered with that customer's details — Priya knows exactly who she's calling and why."

---

## Step 2: Show the Queue

```bash
curl -s http://localhost:8090/api/v1/campaigns/aug-emi-2026/metrics | python3 -m json.tool
```

Expected output at this stage:

```json
{
  "total_accounts": 20,
  "contacted": 0,
  "breakdown": {
    "recovered": 0,
    "promised": 0,
    "refused": 0,
    "no_answer": 0
  },
  "pending": 20,
  "stopped_max_attempts": 0
}
```

---

## Step 3: Live Recovery Call — AGREED path

Open `vasuli/var-thon/aetherRTC/index.html` in the browser. Click **Start Call**.

Priya should speak within 3 seconds. If it takes longer, the LLM was not warmed.

**Expected opening from Priya:**

> "Hello, good morning! This is Priya calling on behalf of [merchant]. May I please speak with Rahul Sharma?"

**How to conduct the demo conversation:**

1. Confirm you are Rahul Sharma
2. Let Priya explain the outstanding ₹4,200
3. Agree to pay: "Yes, please send me the payment link"
4. Priya responds: "I will arrange a payment link to be sent to your registered number shortly."
5. Say "Thank you, goodbye" and end the call

End the call by clicking **End Call** in the browser.

**What to show judges in Terminal 3 logs:**

```
[Recovery] Session assigned: call_session_id=abc-123 → recovery_session_id=rec-001 (Rahul Sharma, ₹4,200)
[Recovery] Call ended: session=abc-123, outcome=AGREED
[Recovery] Payment link created: plink_XXXXXXXX for ₹4,200
```

---

## Step 4: Show the Audit Trail

```bash
sqlite3 vasuli/recovery-orchestrator/vasuli.db \
  "SELECT event_type, json_extract(event_data,'$.outcome'), created_at
   FROM audit_log
   WHERE session_id = 'rec-001'
   ORDER BY created_at;"
```

Expected output:

```
session_assigned  |         | 2026-09-01 10:05:12
call_started      |         | 2026-09-01 10:05:12
call_ended        | AGREED  | 2026-09-01 10:06:08
outcome_classified| AGREED  | 2026-09-01 10:06:09
payment_link_sent |         | 2026-09-01 10:06:09
```

**What to say:** "Every event is timestamped and stored. From the webhook that triggered recovery, through the call, to the payment link dispatch. This is the audit trail the judging bar asks for."

---

## Step 5: Simulate Razorpay payment.captured

```bash
# Compute HMAC signature
BODY='{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_test_001"}}}}'
SIG=$(echo -n "$BODY" | openssl dgst -sha256 -hmac "your_webhook_secret" | awk '{print $2}')

curl -s -X POST http://localhost:8090/webhooks/razorpay \
  -H "Content-Type: application/json" \
  -H "X-Razorpay-Signature: $SIG" \
  -d "$BODY"
```

Then check the recovery session status:

```bash
sqlite3 vasuli/recovery-orchestrator/vasuli.db \
  "SELECT status, recovered_at FROM recovery_sessions WHERE id = 'rec-001';"
```

Expected: `recovered | 2026-09-01 10:07:00`

---

## Step 6: Run the REFUSED path (second call)

Click **Start Call** again. The next customer is assigned automatically (queue-based).

This time, refuse:

- "I don't have money right now."
- "I can't pay." (repeat if needed)
- "Please don't call again."

Expected Priya behavior: acknowledges after the third time, says "I understand, thank you for your time" and does not push further.

**What to show judges:** The stopping rule activates. Check:

```bash
sqlite3 vasuli/recovery-orchestrator/vasuli.db \
  "SELECT status FROM recovery_sessions WHERE call_session_id = 'abc-456';"
# → refused

# Verify this customer will not be assigned again:
curl -X POST http://localhost:8090/api/v1/calls/assign \
  -d '{"call_session_id":"test-probe"}'
# Should return the NEXT customer, not the refused one
```

---

## Step 7: Final Metrics

After running several calls (mix of AGREED, PROMISED, REFUSED):

```bash
curl -s http://localhost:8090/api/v1/campaigns/aug-emi-2026/metrics | python3 -m json.tool
```

```json
{
  "campaign_id": "aug-emi-2026",
  "campaign_name": "August Failed EMI Recovery",
  "total_accounts": 20,
  "contacted": 12,
  "breakdown": {
    "recovered": 5,
    "recovered_amount_paise": 2140000,
    "promised": 4,
    "promised_amount_paise": 1780000,
    "refused": 2,
    "no_answer": 1
  },
  "pending": 8,
  "stopped_max_attempts": 1,
  "payment_links_sent": 5,
  "razorpay_captured_confirmed": 3,
  "generated_at": "2026-09-01T14:30:00Z"
}
```

**What to say:** "Across 12 contacted accounts: 5 recovered totalling ₹21,400, 4 with a promise-to-pay, 2 hard refusals. 3 Razorpay payment.captured webhooks confirmed. 1 account stopped by the max-attempts rule — no further contact will be made. 8 accounts pending."

---

## Timing Targets

| Moment                                            | Target        |
| ------------------------------------------------- | ------------- |
| Click Start Call → Priya speaks first word        | < 3 seconds   |
| Customer utterance ends → Priya begins responding | < 2 seconds   |
| Full recovery call (AGREED path)                  | 45–60 seconds |
| Call ends → audit_log updated                     | < 5 seconds   |
| Call ends → payment link created in Razorpay      | < 10 seconds  |

If Priya's first response takes more than 3 seconds in rehearsal, the LLM was not warmed. Warm it with a curl before the demo.

---

## What to Do if Something Breaks

**Priya doesn't speak after the call connects:**

- Check Terminal 1 (Inference-Python) for errors
- Check that VAD is receiving audio (look for `[VAD] speech started` in logs)
- Most common cause: AetherRTC not running or audio track not negotiated

**Recovery Orchestrator returns empty on assign:**

- Check that the campaign was loaded: `sqlite3 vasuli.db "SELECT count(*) FROM recovery_sessions WHERE status='pending';"`
- If 0: re-run the campaign load curl

**Outcome classification returns UNCLEAR:**

- Acceptable. Have a manual fallback: `curl -X POST .../api/v1/calls/<id>/end -d '{"outcome":"AGREED"}'`
- Mention to judges: "For this call the classifier was uncertain — production would include a human review queue for UNCLEAR cases. I've manually marked it for the demo."

**Payment link creation fails:**

- Check Razorpay API keys are test-mode (not live)
- Check `razorpay-key-id` starts with `rzp_test_`
