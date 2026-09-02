# Recovery Orchestrator

The business-logic service in [Vasuli](../README.md), campaign management,
stopping rules, outcome handling, and the audit trail. This is the only
service in the system that knows about customers, campaigns, or Razorpay.
It has no knowledge of voice, WebRTC, or AI inference; that lives entirely
in [`var-thon`](../var-thon).

> **Status: Active development.** The core service, campaign creation,
> queue-based session assignment, outcome handling (AGREED/PROMISED/REFUSED/
> UNCLEAR), and the audit trail, is built and verified end-to-end. It is
> now wired to `var-thon`: a real browser call, routed through the full
> voice pipeline, spoke a dynamically-assigned customer's name and payment
> details live. The Razorpay integration is currently a stub, by design,
> see [Status](#status) below.

---

## Why this exists

Vasuli's voice pipeline (`var-thon`) manages a single call's lifecycle,
audio in, audio out, one conversation. It has no concept of a *batch* of
customers, no concept of a payment being overdue, and no concept of what to
do once a call ends. Recovery Orchestrator is the layer above that: it owns
the queue of customers to contact, decides which one to hand to the next
incoming call, and turns a call's outcome into a concrete business action,
send a payment link, log a promised date, or permanently stop contacting
someone who refused.

Full system architecture and the reasoning behind this split live in
[`../docs/architecture.md`](../docs/architecture.md).

---

## Architecture

```text
┌─────────────────────────────────────────────────────────────┐
│                    RECOVERY ORCHESTRATOR                    │
│                                                               │
│   api/            HTTP handlers, decode, delegate, encode   │
│     │                                                        │
│     ▼                                                        │
│   campaign/        Business rules: stopping rules, outcome   │
│     │               handling, system prompt rendering        │
│     │                                                        │
│     ├──────────────┐                                         │
│     ▼              ▼                                         │
│   store/          razorpay/                                  │
│   SQLite,          Payment link creation, behind an           │
│   audit trail       interface (Stub today, Live in M6)        │
└─────────────────────────────────────────────────────────────┘
```

Each package has one job and talks to its neighbors through a narrow
interface:

- **`internal/api`**, HTTP only. Decodes JSON, calls into `campaign`,
  encodes the response. No business logic.
- **`internal/campaign`**, the actual rules: which session gets assigned
  next, what happens when a call ends with a given outcome, how a system
  prompt gets rendered for a customer. Knows nothing about HTTP or SQL.
- **`internal/store`**, the only package that writes SQL. Typed methods in,
  typed structs out; no raw `*sql.DB` leaks past this package.
- **`internal/razorpay`**, payment-link creation behind a `Client`
  interface. `StubClient` today; a `LiveClient` making real Razorpay API
  calls arrives once test-mode credentials exist (M6).

---

## Running it

```bash
go run ./cmd -port :8090 -db ./vasuli.db
```

| Flag | Env fallback | Default | Meaning |
| ---- | ------------ | ------- | ------- |
| `-port` |, | `:8090` | HTTP listen address |
| `-db` |, | `./vasuli.db` | SQLite database path (created if absent) |
| `-razorpay-key-id` | `RAZORPAY_KEY_ID` | *(empty)* | Test-mode key ID. Empty uses the stub client |
| `-razorpay-key-secret` | `RAZORPAY_KEY_SECRET` | *(empty)* | Test-mode key secret |
| `-razorpay-webhook-secret` | `RAZORPAY_WEBHOOK_SECRET` | *(empty)* | Shared secret inbound webhooks are signed with |

Credentials resolve **flag > shell environment > `.env`**. A `.env` file in
the working directory is loaded at startup; it is gitignored and must not
be committed. Each flag's default is read from the environment, so
precedence needs no merge logic.

On startup, the service applies its schema (idempotent, safe to run
against an existing database) and starts listening.

With no Razorpay credentials it runs fully standalone against
`StubClient`, which fabricates payment links without any network call,
every endpoint including the webhook consumer works in that mode. Supply
both key flags to switch to the real test-mode API. **Startup refuses any
key without an `rzp_test_` prefix**: Vasuli generates payment demands
autonomously from an AI conversation, and the distance between test and
live mode is a single flag.

Without `-razorpay-webhook-secret`, webhook verification fails closed and
every inbound webhook is rejected.

---

## API

All request and response bodies are JSON.

### `POST /api/v1/campaigns`

Create a campaign and bulk-insert its accounts. Renders a per-customer
system prompt for each account at creation time, `var-thon` never does
template substitution itself.

**Request**, amounts are whole rupees:

```json
{
  "name": "August Failed EMI Recovery",
  "accounts": [
    {
      "customer_name": "Rahul Sharma",
      "outstanding_rupees": 4200,
      "product_name": "Bajaj Finance Personal Loan",
      "due_date": "2026-08-15",
      "razorpay_payment_id": "pay_test_001"
    }
  ]
}
```

Rupees appear only at this boundary. Internally, and in every call to
Razorpay, money is an integer count of paise, the smallest unit, which is
what Razorpay's API expects and what keeps floating point out of currency
arithmetic.

**Response, `201 Created`**

```json
{
  "campaign_id": "camp_f474c969503c98f4",
  "name": "August Failed EMI Recovery",
  "total": 1,
  "status": "active",
  "created_at": "2026-08-23T06:24:10Z"
}
```

### `POST /api/v1/calls/assign`

Called by `var-thon` immediately after `START_SESSION`. Pops the oldest
eligible pending session (FIFO, respecting stopping rules) and binds it to
the given `call_session_id`.

**Request**

```json
{ "call_session_id": "webrtc_session_001" }
```

**Response, `200 OK`**

```json
{
  "session_id": "rec_2eed6a210156b1a9",
  "customer_name": "Rahul Sharma",
  "outstanding_amount_paise": 420000,
  "product_name": "Bajaj Finance Personal Loan",
  "due_date": "2026-08-15",
  "system_prompt": "You are Priya, a professional payment recovery specialist..."
}
```

**Response, `404 Not Found`**, the queue is empty. This is an expected,
non-error condition; `var-thon` falls back to its static agent profile.

### `POST /api/v1/calls/{call_session_id}/end`

Called by `var-thon` when a session ends. Records the outcome and applies
its consequence.

**Request**

```json
{ "outcome": "AGREED", "promise_date": null }
```

`outcome` is one of `AGREED`, `PROMISED`, `REFUSED`, or `UNCLEAR`.

| Outcome    | Effect                                                                 |
| ---------- | ----------------------------------------------------------------------- |
| `AGREED`   | Creates a payment link (via `razorpay.Client`), status → `link_sent`   |
| `PROMISED` | Stores `promise_date`, status → `promised`                             |
| `REFUSED`  | Status → `refused`, permanently excluded from future assignment        |
| `UNCLEAR`  | Status → `unclear` if attempts remain, else `failed` (stopping rule)   |

**Response, `200 OK`**

```json
{ "status": "ok" }
```

### `GET /api/v1/campaigns/{id}/metrics`

Aggregate counts and amounts for a campaign, computed live from
`recovery_sessions`, never cached, never hardcoded.

**Response, `200 OK`**

```json
{
  "campaign_id": "camp_f474c969503c98f4",
  "campaign_name": "August Failed EMI Recovery",
  "total_accounts": 20,
  "contacted": 12,
  "breakdown": {
    "recovered": 5,
    "recovered_amount_paise": 2140000,
    "promised": 4,
    "promised_amount_paise": 1780000,
    "refused": 2,
    "unclear": 1
  },
  "pending": 8,
  "stopped_max_attempts": 0,
  "payment_links_sent": 5,
  "razorpay_captured_confirmed": 0
}
```

### `GET /api/v1/campaigns/{id}/queue`

Who the system will call, who it is waiting on, and who it has finished
with. Read-only; it does not consume the queue.

```json
{
  "campaign_id": "camp_...",
  "generated_at": "2026-09-02T14:30:00Z",
  "due_now": [
    {"customer_name":"Rahul Sharma","outstanding_rupees":4200,"status":"pending","attempt":"0 of 3"}
  ],
  "on_hold": [
    {"customer_name":"Preethi Nair","status":"unclear","attempt":"1 of 3","next_eligible_at":"2026-09-03T14:22:00Z"}
  ],
  "closed": [
    {"customer_name":"Vikram Rao","status":"refused","reason":"escalated to human agent"}
  ]
}
```

Buckets come from the same `eligibleNow` predicate the assignment query
uses, so this view cannot claim a customer is due while the assigner skips
them.

### `POST /webhooks/razorpay`

Consumes Razorpay webhooks. The raw request body is verified against
`X-Razorpay-Signature` (HMAC-SHA256) **before** parsing, decoding and
re-encoding the JSON changes the bytes and therefore the signature.

| Event | Matched on | Action |
| ----- | ---------- | ------ |
| `payment.failed` |, | Creates a session on the most recent active campaign |
| `payment.captured` | `razorpay_payment_id` | Marks the original failed payment recovered |
| `payment_link.paid` | `razorpay_link_id` | Marks a Vasuli-generated link recovered |
| anything else |, | Acknowledged, no action |

`payment.captured` and `payment_link.paid` are **not** interchangeable. A
customer paying a link Vasuli generated produces a *new* payment id with
no relationship to the failed payment that started recovery, so only the
link id can resolve it.

**Responses** are chosen for how Razorpay reacts to them, any non-2xx
triggers retries with backoff:

- `400`. Signature verification failed, or malformed payload. Retrying
  cannot help.
- `200`. Processed, or acknowledged-and-ignored (unknown session, no
  active campaign, unactionable event type). Prevents infinite redelivery.

All handlers are idempotent; redelivery is the normal case, not an edge
case.

**Testing without a Razorpay account:** the webhook secret is a string you
choose and pass to both the server and your `curl`. No dashboard webhook
registration is needed.

```bash
BODY='{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_test_001"}}}}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "your_webhook_secret" | awk '{print $2}')

curl -X POST http://localhost:8090/webhooks/razorpay \
  -H "Content-Type: application/json" \
  -H "X-Razorpay-Signature: $SIG" \
  -d "$BODY"
```

### `GET /health`

```json
{ "status": "ok", "db": "connected" }
```

---

## Data model

Three tables, one SQLite file. Full schema in
[`internal/store/schema.sql`](internal/store/schema.sql).

```text
campaigns, a batch of accounts to contact
recovery_sessions, one row per customer per recovery attempt
audit_log, append-only event log; the audit trail
```

`recovery_sessions.status` lifecycle:

```text
pending → active → { link_sent | promised | refused | unclear | failed }
                                  link_sent → recovered (on payment.captured)
```

Every state transition writes an immutable row to `audit_log`. For a given
call, the full trail is queryable directly:

```bash
sqlite3 vasuli.db \
  "SELECT event_type, event_data, created_at FROM audit_log
   WHERE session_id = 'rec_...' ORDER BY created_at;"
```

```text
session_created     {"customer_name":"Rahul Sharma"}
session_assigned    {"call_session_id":"webrtc_session_001"}
call_ended          {"outcome":"AGREED"}
outcome_classified  {"outcome":"AGREED"}
payment_link_sent   {"razorpay_link_id":"plink_...","amount_paise":420000}
```

---

## Design decisions worth knowing before changing this code

**`SetMaxOpenConns(1)` in `store.Open`.** SQLite allows exactly one writer
at a time; Go's `database/sql` pool defaults to assuming a real
multi-connection server database. Left at its default, two goroutines could
each read the same "pending" row before either writes it back as "active,"
double-assigning one customer to two concurrent calls. Pinning the pool to
one connection makes `database/sql` itself serialize access, closing the
race by configuration rather than hand-rolled locking.

**`razorpay.Client` is an interface with one method.** Not speculative
abstraction. There are exactly two known implementations (`StubClient` now,
a `LiveClient` once M6 has test-mode credentials), and `campaign.Manager`
needs to compile and be fully exercisable today without real credentials.

**Webhooks are simulated, not tunnelled.** Razorpay cannot reach
`localhost`, so demonstrating inbound events needs either a public tunnel
or locally-signed requests. The verification code is identical in both
cases, and a tunnel adds an external dependency that can fail during a
live demo. So the demo signs its own requests with a shared secret.

**`payment.failed` with no active campaign is dropped, not stored.** It's
acknowledged with 200 (so Razorpay stops retrying) and logged. The
alternative. Auto-creating a campaign to own the session, makes campaign
ownership ambiguous, and that ambiguity resurfaces later as metrics that
reconcile against no batch anyone loaded.

**Standard library router, no framework.** Five endpoints don't justify a
routing dependency; Go 1.22+'s `http.ServeMux` method+path patterns
(`"POST /api/v1/calls/{call_session_id}/end"`) are sufficient.

---

## Status

Tracking [`../docs/roadmap.md`](../docs/roadmap.md)'s milestones, built as
vertical slices rather than strictly sequentially.

**Done:**

- [x] Campaign creation with per-customer system prompt rendering
- [x] Queue-based session assignment (FIFO, atomic, race-safe)
- [x] Outcome handling for all four outcomes (AGREED/PROMISED/REFUSED/UNCLEAR)
- [x] Stopping rules: permanent refusal exclusion, max-attempts exhaustion
- [x] Full audit trail matching the documented demo query
- [x] Campaign metrics, computed live
- [x] Razorpay payment-link creation behind a stub (real calls in M6)
- [x] Verified end-to-end via live HTTP smoke test (not just unit-level)
- [x] `var-thon` wiring (`internal/recovery/client.go`, per-session profile
      resolution in `gateway/server.go`), verified with a real browser call
      through the full voice pipeline; see `../docs/roadmap.md` M3

- [x] Razorpay webhook consumer (`POST /webhooks/razorpay`), HMAC-SHA256
      verification, idempotent handling of `payment.failed`,
      `payment.captured`, and `payment_link.paid`
- [x] `razorpay.LiveClient`, real payment-link creation, selected over the
      stub when credentials are supplied; startup refuses non-test keys
- [x] Go unit tests for signature verification, payload parsing, and the
      live client's request shape

- [x] Follow-up scheduling. Every outcome decides when the customer may be
      contacted again, or that they never may be
- [x] Queue view showing due / on hold / closed, from the same predicate the
      assigner uses

**Not yet built:**

- [ ] Outcome signal from Inference-Python back to Orchestrator-Go
- [ ] Synthetic 20-account batch and a full rehearsal run
- [ ] Cooldown-based re-queueing (currently `UNCLEAR` either retries once
      the attempt budget allows or fails permanently, no time-based
      cooldown window yet; not required for the current demo scope)

---

## Project structure

```text
recovery-orchestrator/
├── cmd/main.go                      Composition root: DB, Razorpay client,
│                                     campaign manager, router, listener
├── cmd/env.go                       .env loader (no external dependency)
├── internal/
│   ├── api/
│   │   ├── router.go                stdlib http.ServeMux routing table
│   │   └── handlers.go              JSON decode/encode, no business logic
│   ├── campaign/
│   │   └── manager.go               Stopping rules, outcome handling,
│   │                                 system prompt rendering
│   ├── razorpay/
│   │   └── client.go                Client interface + StubClient
│   └── store/
│       ├── schema.sql                Table definitions
│       └── db.go                     All SQL access
├── go.mod / go.sum
└── testdata/                         Synthetic campaign fixtures (M7)
```

---

## License

Apache 2.0, see [`../LICENSE`](../LICENSE).
