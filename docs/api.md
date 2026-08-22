# Razorpay Integration Reference

Vasuli uses Razorpay's test-mode APIs in two directions: inbound webhooks that trigger recovery campaigns, and outbound API calls that create payment links after successful calls.

All interactions are strictly test-mode. No real money moves.

---

## Credentials Setup

From the Razorpay Dashboard (test mode):

1. **API Keys:** Settings → API Keys → Generate Test Key → copy `key_id` and `key_secret`
2. **Webhook Secret:** Settings → Webhooks → Add Webhook → set URL to `http://localhost:8090/webhooks/razorpay` → copy the generated secret

Pass credentials to the Recovery Orchestrator at startup:

```bash
go run ./cmd/main.go \
  -razorpay-key-id    rzp_test_XXXXXXXXXXXX \
  -razorpay-key-secret YYYYYYYYYYYYYYYYYYYY \
  -razorpay-webhook-secret ZZZZZZZZZZZZZZZZ
```

---

## Inbound: Webhook Consumer

**Endpoint:** `POST /webhooks/razorpay`

### Signature Verification

Every incoming webhook must be verified before processing. Razorpay signs the raw request body with the webhook secret using HMAC-SHA256 and sends the signature in the `X-Razorpay-Signature` header.

```go
func verifyWebhookSignature(body []byte, signature, secret string) bool {
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    expected := hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(expected), []byte(signature))
}
```

Reject any request where signature verification fails with HTTP 400. Do not log the body of failed requests (it may contain customer data from an attacker).

### Event Types

| Event              | Action                                             |
| ------------------ | -------------------------------------------------- |
| `payment.failed`   | Create a `recovery_session` for the failed payment |
| `payment.captured` | Mark matching `recovery_session` as recovered      |
| All others         | Acknowledge with 200, take no action               |

### payment.failed payload (relevant fields)

```json
{
  "event": "payment.failed",
  "payload": {
    "payment": {
      "entity": {
        "id": "pay_XXXXXXXXXXXXXXXX",
        "amount": 420000,
        "currency": "INR",
        "description": "Bajaj Finance Personal Loan EMI",
        "email": "rahul.sharma@example.com",
        "contact": "+919876543210",
        "notes": {
          "customer_name": "Rahul Sharma"
        },
        "error_code": "BAD_REQUEST_ERROR",
        "error_description": "Your card has insufficient funds."
      }
    }
  }
}
```

On receiving this, the Recovery Orchestrator:

1. Extracts `payment.entity.id`, `amount`, `description`, `notes.customer_name`
2. Creates a `recovery_session` with status=`pending`
3. Renders the system prompt template with extracted fields
4. Writes `audit_log`: `event_type=webhook_received`, `event_data={"razorpay_payment_id":"pay_..."}`

### payment.captured payload (relevant fields)

```json
{
  "event": "payment.captured",
  "payload": {
    "payment": {
      "entity": {
        "id": "pay_YYYYYYYYYYYYYY"
      }
    }
  }
}
```

On receiving this, the Recovery Orchestrator:

1. Queries `recovery_sessions WHERE razorpay_payment_id = 'pay_YYY...'`
2. If found and status != `recovered`: update status=`recovered`, set `recovered_at`
3. Writes `audit_log`: `event_type=payment_captured`

### Simulating Webhooks for the Demo

In test-mode, Razorpay Dashboard → Webhooks → "Test Payload" lets you fire synthetic events. Alternatively, use curl directly once you have the webhook secret to construct a valid HMAC:

```bash
# Compute HMAC (requires openssl)
BODY='{"event":"payment.failed","payload":{"payment":{"entity":{"id":"pay_testXXXXXXXX","amount":420000,"currency":"INR","description":"Test EMI","notes":{"customer_name":"Rahul Sharma"}}}}}'
SIG=$(echo -n "$BODY" | openssl dgst -sha256 -hmac "your_webhook_secret" | awk '{print $2}')

curl -X POST http://localhost:8090/webhooks/razorpay \
  -H "Content-Type: application/json" \
  -H "X-Razorpay-Signature: $SIG" \
  -d "$BODY"
```

---

## Outbound: Payment Link Creation

**Called by:** Recovery Orchestrator, after a call ends with outcome=AGREED.

**Razorpay API:**

```
POST https://api.razorpay.com/v1/payment_links
Authorization: Basic base64(key_id + ":" + key_secret)
Content-Type: application/json
```

**Request body:**

```json
{
  "amount": 420000,
  "currency": "INR",
  "description": "Payment recovery — Bajaj Finance Personal Loan",
  "customer": {
    "name": "Rahul Sharma"
  },
  "notify": {
    "sms": false,
    "email": false
  },
  "reminder_enable": false,
  "expire_by": 1757030400
}
```

- `amount` is in paise (₹4,200 = 420000 paise)
- `notify.sms` and `notify.email` are false — in the demo, no real SMS/email is sent
- `expire_by` is a Unix timestamp 24 hours from now (so the link doesn't linger indefinitely in test mode)

**Response (relevant fields):**

```json
{
  "id": "plink_XXXXXXXXXXXXXXXX",
  "short_url": "https://rzp.io/i/XXXXXX",
  "status": "created"
}
```

Store `response.id` as `recovery_sessions.razorpay_link_id`.

Log to `audit_log`:

```json
{
  "event_type": "payment_link_sent",
  "event_data": {
    "razorpay_link_id": "plink_XXXXXXXXXXXXXXXX",
    "short_url": "https://rzp.io/i/XXXXXX",
    "amount_paise": 420000
  }
}
```

---

## Synthetic Campaign Data Format

For the demo, campaigns are loaded via `POST /api/v1/campaigns` with a JSON body.

```json
{
  "name": "August Failed EMI Recovery",
  "accounts": [
    {
      "customer_name": "Rahul Sharma",
      "outstanding_paise": 420000,
      "product_name": "Bajaj Finance Personal Loan",
      "due_date": "2026-07-15",
      "razorpay_payment_id": "pay_test_001"
    },
    {
      "customer_name": "Preethi Nair",
      "outstanding_paise": 189000,
      "product_name": "Airtel Postpaid Bill",
      "due_date": "2026-08-01",
      "razorpay_payment_id": "pay_test_002"
    }
  ]
}
```

The Recovery Orchestrator's campaign manager:

1. Creates a `campaigns` row
2. For each account: renders the system prompt template, inserts a `recovery_sessions` row with status=`pending`
3. Writes `audit_log` per session: `event_type=session_created`

The system prompt template (Go `text/template`):

```
You are Priya, a professional payment recovery specialist calling on behalf
of a Razorpay merchant.

You are calling {{.CustomerName}} regarding an outstanding payment of
₹{{.AmountFormatted}} for {{.ProductName}} that was due on {{.DueDate}}.

Your objectives in order:
1. Greet {{.CustomerName}} and confirm you are speaking with them.
2. State the outstanding amount clearly: ₹{{.AmountFormatted}} for {{.ProductName}}.
3. Offer to help them pay now — you will arrange a payment link.
4. If they cannot pay now, get a specific date and confirm it back to them.
5. If they decline, acknowledge respectfully and close.

Rules:
- Never be aggressive, threatening, or pressuring.
- Do not mention the payment more than three times if refused.
- If they agree: say exactly "I will arrange a payment link to be sent
  to your registered number shortly."
- If they give a date: repeat it back — "So you will pay by [date],
  I have noted that down."
- If they refuse: say "I understand, thank you for your time."
- Keep the call under 3 minutes.
- Speak professional Indian English.
```

---

## What Test-Mode Covers

| Feature                          | Test-mode support                             |
| -------------------------------- | --------------------------------------------- |
| Webhook delivery (via Dashboard) | Yes                                           |
| Webhook signature verification   | Yes (same HMAC logic as production)           |
| Payment link creation            | Yes (links are real but no real payment)      |
| `payment.captured` event         | Yes (trigger manually from Dashboard or curl) |
| Real SMS/email notification      | No (disabled in our payload)                  |
| Real money transfer              | No                                            |

Everything in the demo uses test-mode throughout. The integration logic (HMAC verification, API calls, event parsing) is identical to what production would require — only the credentials differ.
