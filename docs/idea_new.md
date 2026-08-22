## What You're Actually Starting With

This matters because it determines what you trust and what needs verification on the fork.

**Verified and working in VAR today:**

- Three-tier pipeline: browser → AetherRTC → Orchestrator-Go → Inference-Python, end-to-end with real audio
- VAD (Silero ONNX, four-state debounce machine, verified against false starts, ramble, state persistence)
- STT pinned to English, sub-1.1s end-to-end latency
- LLM via Ollama with streaming token delivery and sentence-boundary TTS
- Dynamic profiling — YAML system prompt flows through to LLM via `ControlSignal`
- Per-session conversation history in `SessionContext`
- Session state machine with mutex-protected transitions and `sync.Once` on `DoneChan`

**Known open items that are not blockers for the hackathon fork:**

- Milestone 6: goroutine leak verification and session_id tracing across all three logs. This is VAR's problem, not var-thon's — you'll discover any leak during your first end-to-end test run anyway.
- Monitor goroutine (barge-in). Not needed for a structured recovery call.
- Phase 2 (tool calling, RAG). Not needed — post-call LLM classification handles outcome detection instead.

**What AetherRTC actually implements** (LLD is aspirational in places):

- The LLD says Opus. The actual implementation uses G.711/PCMU (`pkg/codec/g117.go`). The LLD's `pkg/codec/opus.go` is a placeholder stub.
- G.711 decode/encode is verified working with real browser audio.
- AetherRTC is genuinely unchanged for this project. Don't touch it.

---

## The Three-Service Architecture

```
recovery-orchestrator          (new Go service — the entire hackathon build)
    │
    ├── drives: batch campaigns, stopping rules, audit trail, Razorpay API
    │
    ├── called by: var-thon Orchestrator-Go (at session start and end)
    │
    └── calls: Razorpay test-mode APIs

var-thon                       (fork of VAR — one change in gateway/server.go)
    │
    ├── Orchestrator-Go: calls recovery-orchestrator for per-session context
    ├── Inference-Python: unchanged, picks up dynamic system_prompt already
    └── New YAML: configs/agent_profiles/recovery_agent.yaml

AetherRTC                      (original repo, not forked, not changed)
    └── runs exactly as today, pointed at var-thon's Orchestrator-Go :50052
```

---

## Repository Structure After Forking

```
var-thon/                                   ← fork of VAR
├── proto/
│   ├── agent.proto                         (unchanged)
│   └── gateway.proto                       (unchanged)
├── services/
│   ├── orchestrator-go/
│   │   ├── cmd/
│   │   │   └── gateway-server/main.go      (unchanged)
│   │   └── internal/
│   │       ├── config/config.go            (unchanged)
│   │       ├── gateway/server.go           ← ONE CHANGE HERE
│   │       ├── recovery/client.go          ← NEW FILE
│   │       └── session/session.go          (unchanged)
│   └── inference-py/
│       └── main.py                         (unchanged)
└── configs/
    └── agent_profiles/
        ├── receptionist.yaml               (unchanged)
        └── recovery_agent.yaml             ← NEW FILE

recovery-orchestrator/                      ← entirely new service
├── cmd/
│   └── main.go
├── internal/
│   ├── api/
│   │   ├── handlers.go
│   │   └── router.go
│   ├── campaign/
│   │   └── manager.go
│   ├── razorpay/
│   │   └── client.go
│   └── store/
│       ├── db.go
│       └── schema.sql
├── go.mod
└── go.sum
```

---

## Milestone Plan

These are ordered by dependency. Each has a concrete definition of done.

### M1 — Fork and verify baseline

Fork VAR as `var-thon`. Run the full stack: Inference-Python → Orchestrator-Go gateway-server → AetherRTC → browser. Open a tab, speak, hear a response. If that works, the fork is clean.

**Done when:** Receptionist profile responds in a live browser tab on the fork. No new code written yet.

---

### M2 — Recovery Orchestrator skeleton

New Go service. HTTP server with four endpoints wired up and SQLite initialized.

Endpoints:

- `POST /api/v1/campaigns` — create campaign, bulk-insert synthetic accounts
- `GET /api/v1/campaigns/:id` — return campaign status + metrics
- `POST /api/v1/calls/assign` — pop next pending recovery_session, bind call_session_id, return context JSON
- `POST /api/v1/calls/:session_id/end` — mark call ended, write outcome

The `/api/v1/calls/assign` response shape:

```json
{
  "session_id": "rec-uuid-001",
  "customer_name": "Rahul Sharma",
  "outstanding_amount_paise": 420000,
  "product_name": "Bajaj Finance Personal Loan",
  "due_date": "2026-08-15",
  "system_prompt": "You are Priya, a professional payment recovery..."
}
```

The system_prompt field is fully rendered — customer name, amount, dates already substituted. No templating happens on var-thon's side.

**Done when:** `curl -X POST localhost:8090/api/v1/campaigns` creates a campaign with 5 hardcoded synthetic accounts, and `curl -X POST localhost:8090/api/v1/calls/assign` returns the first pending account's context.

---

### M3 — Wire var-thon to Recovery Orchestrator

Two new files in var-thon's Orchestrator-Go.

`internal/recovery/client.go` — thin HTTP client:

```go
type Client struct {
    baseURL    string
    httpClient *http.Client
}

type SessionContext struct {
    SessionID             string `json:"session_id"`
    CustomerName          string `json:"customer_name"`
    OutstandingPaise      int64  `json:"outstanding_amount_paise"`
    ProductName           string `json:"product_name"`
    DueDate               string `json:"due_date"`
    SystemPrompt          string `json:"system_prompt"`
}

func (c *Client) AssignSession(callSessionID string) (*SessionContext, error)
func (c *Client) EndSession(callSessionID, outcome string) error
```

`internal/gateway/server.go` change — after receiving `START_SESSION`:

```go
// existing: profile loaded from config at startup
// var-thon: attempt dynamic context from recovery orchestrator

if s.RecoveryClient != nil {
    ctx, err := s.RecoveryClient.AssignSession(sessionID)
    if err == nil && ctx != nil {
        profile = &config.AgentProfile{
            Name:         ctx.CustomerName,
            SystemPrompt: ctx.SystemPrompt,
        }
        log.Printf("[Gateway] Recovery context assigned for session %s (customer: %s)", sessionID, ctx.CustomerName)
    } else {
        log.Printf("[Gateway] No recovery context for session %s, using static profile: %v", sessionID, err)
    }
}
```

Fallback to static profile if Recovery Orchestrator is down or queue is empty. Session always proceeds.

At `END_SESSION` / when `sess.DoneChan` closes:

```go
if s.RecoveryClient != nil {
    s.RecoveryClient.EndSession(sessionID, "completed")
}
```

Outcome classification happens in Python and gets sent separately (M5).

**Done when:** Open a browser tab, speak, and the agent introduces itself using the customer name from the Recovery Orchestrator's first queued account. Check that the audit_log has a `session_assigned` row.

---

### M4 — Recovery agent profile

`configs/agent_profiles/recovery_agent.yaml` in var-thon. This is the fallback when Recovery Orchestrator has no pending session. The actual per-call system prompt is generated dynamically in the Recovery Orchestrator and overrides this — but this file needs to exist and be loadable.

The system prompt (generation template, used in Recovery Orchestrator's `campaign/manager.go`):

```
You are Priya, a professional payment recovery specialist calling on behalf of a Razorpay merchant.

You are calling {{.CustomerName}} regarding an outstanding payment of ₹{{.AmountFormatted}} for {{.ProductName}} that was due on {{.DueDate}}.

Your objectives in order:
1. Greet the customer and confirm you are speaking with {{.CustomerName}}.
2. State the outstanding amount clearly and explain it is overdue.
3. Offer to help them pay now — you will arrange a payment link to their registered number.
4. If they cannot pay now, ask for a specific date when they can pay and confirm it back to them.
5. If they decline entirely, acknowledge politely and close the call.

Rules you must follow:
- Never be aggressive, threatening, or pressuring. Always be empathetic.
- Do not mention the payment more than three times if the customer has declined.
- If the customer agrees to pay: say exactly "I will arrange a payment link to be sent to your registered number shortly."
- If the customer gives a date: repeat it back — "So you will be able to pay by [date], I have noted that."
- If they refuse: say "I understand, thank you for your time" and end the call.
- Keep the total call under 3 minutes.
- Speak professional Indian English. Be warm but businesslike.
```

**Done when:** Test call where Recovery Orchestrator queue is empty falls back cleanly to this profile and the agent still functions correctly.

---

### M5 — Post-call outcome classification

In var-thon's `inference-py/main.py`, at the end of `_utterance_worker` when `_UTTERANCE_STOP` is received (i.e., the call is ending):

```python
def _classify_outcome(self, ctx: SessionContext) -> str:
    if not ctx.conversation_history:
        return "UNCLEAR"

    messages = [
        {"role": "system", "content": "You classify call outcomes. Reply with exactly one word."},
        *ctx.conversation_history,
        {
            "role": "user",
            "content": (
                "Based on this recovery call conversation, what was the customer's final response?\n"
                "Reply with exactly one word:\n"
                "AGREED — customer agreed to pay now\n"
                "PROMISED — customer committed to a specific future date\n"
                "REFUSED — customer declined to pay\n"
                "UNCLEAR — no clear resolution"
            ),
        },
    ]
    result = self.llm.generate(
        prompt="",
        system_override=None,
        history=messages[:-1],
    )
    word = result.strip().upper().split()[0] if result.strip() else "UNCLEAR"
    if word not in ("AGREED", "PROMISED", "REFUSED", "UNCLEAR"):
        return "UNCLEAR"
    return word
```

This runs after the last `_run_utterance` completes. The outcome gets passed to var-thon's Orchestrator-Go via a new field in the `END_SESSION` call body.

Orchestrator-Go `EndSession` call now includes:

```json
{
  "outcome": "AGREED",
  "promise_date": null
}
```

Recovery Orchestrator receives this, updates `recovery_sessions.status`, and if `AGREED`, calls Razorpay test-mode to create a payment link.

**Done when:** Run a call where you say "okay send me the link." Verify audit_log shows `call_ended` with `outcome: AGREED`. Verify `recovery_sessions.status = 'recovered'` after `payment.captured` webhook fires (simulated).

---

### M6 — Razorpay test-mode integration

Two Razorpay interactions in `recovery-orchestrator/internal/razorpay/client.go`:

**Inbound — webhook consumer:**

```
POST /webhooks/razorpay
Verify X-Razorpay-Signature (HMAC-SHA256 of raw body with webhook secret)
Parse event type: payment.failed → create recovery_session
               payment.captured → mark recovery_session as recovered
```

For the batch demo, you don't need real failed payments coming in. You seed the database directly via `POST /api/v1/campaigns` with synthetic records. The webhook consumer demonstrates that the system _could_ be triggered by real Razorpay events — you simulate one during the demo.

**Outbound — payment link creation:**

```
POST https://api.razorpay.com/v1/payment_links
Authorization: Basic base64(key_id:key_secret)
{
  "amount": 420000,
  "currency": "INR",
  "description": "Payment recovery - Bajaj Finance Personal Loan",
  "customer": {
    "name": "Rahul Sharma"
  },
  "notify": { "sms": false, "email": false }
}
```

Test-mode key from Razorpay dashboard. Store `razorpay_link_id` from response.

**Done when:** A completed call with `AGREED` outcome results in a Razorpay test-mode payment link being created and the link ID stored in the database.

---

### M7 — Metrics endpoint and batch simulation

`GET /api/v1/campaigns/:id/metrics` returns:

```json
{
  "campaign_id": "aug-emi-recovery",
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
  "generated_at": "2026-08-22T14:30:00Z"
}
```

Also add a simple formatted text output for the demo (judges see this in terminal):

```
Campaign: August Failed EMI Recovery
────────────────────────────────────────
Total accounts:    20
Contacted:         12

  Recovered:        5   ₹21,400
  Promised:         4   ₹17,800 (pending)
  Refused:          2
  No answer:        1

Pending contact:   8
Stopped:           1   (max attempts reached)
────────────────────────────────────────
Payment links sent:          5
Razorpay confirmed:          3   ₹12,600
────────────────────────────────────────
Audit trail: recovery.db
```

Load 20 synthetic accounts with realistic Indian names, amounts (₹2,000–₹25,000), product names (personal loans, EMIs, subscriptions). Manually run 5-6 calls with different outcomes. The remaining 14 stay as `pending` — that's honest, not gamed.

**Done when:** `GET /api/v1/campaigns/aug-emi-recovery/metrics` returns real numbers from real calls made during testing.

---

### M8 — Demo rehearsal

Run the exact demo sequence twice before the submission date:

1. Load campaign via API. Show the pending queue.
2. Open browser tab, speak one full recovery call (aim for 45-60 seconds). Cover the "customer agrees" path.
3. Show the audit trail for that call: `SELECT * FROM audit_log WHERE session_id = '...' ORDER BY created_at`.
4. Show the payment link created in Razorpay test-mode dashboard.
5. Run `GET /api/v1/campaigns/:id/metrics`. Show the numbers.
6. Simulate one `payment.captured` webhook. Show the status update.

Time the voice call section. If the agent's opening line takes more than 3 seconds to start speaking, the first impression is broken. Warm Ollama before the demo (`keep_alive=-1` is already in the LLM config — verify it's set).

---

## The Order to Do This

Start with M2 (Recovery Orchestrator skeleton) in parallel with M1 (fork and verify). These have no dependency on each other. M3 (wiring) depends on both M1 and M2 being done. Everything after M3 is sequential.

```
M1 (fork + verify) ─────────────────────┐
                                         ├── M3 (wire) → M4 → M5 → M6 → M7 → M8
M2 (orchestrator skeleton) ─────────────┘
```
