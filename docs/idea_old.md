## Fork AetherRTC: No

Here's the precise reasoning.

The session_id problem (Recovery Orchestrator needs to pre-know the session_id to inject context) is solvable without touching AetherRTC at all. The wrong solution is "let the browser pass its own session_id into AetherRTC's signaling" — that's a change to the signaling handshake, adds a branch in `signaling/server.go`, and requires you to maintain a fork of a service that genuinely doesn't need to change.

The right solution is **queue-based session assignment**. Instead of pre-registering a session_id in the Recovery Orchestrator, var-thon calls the Recovery Orchestrator _after_ AetherRTC generates the session_id, and the Recovery Orchestrator assigns the next pending customer to that call:

```
AetherRTC generates session_id → passes to var-thon via gateway.proto
var-thon Orchestrator-Go receives START_SESSION(session_id="abc-123")
var-thon → POST recovery-orchestrator/api/v1/calls/assign {session_id: "abc-123"}
Recovery Orchestrator → pops next pending customer from queue, binds "abc-123" to them
Returns → {customer_name, amount, system_prompt}
var-thon uses that system_prompt for the session
```

AetherRTC never changes. Original repo, original binary. One less thing to maintain during a hackathon.

---

## The Full System: 3 Services

```
┌─────────────────────────────────────────────────────────────────────┐
│                     RECOVERY ORCHESTRATOR                           │
│                       (new Go service)                              │
│                                                                     │
│  Webhook Consumer     Batch/Campaign Manager    Audit Store         │
│  POST /webhooks/      POST /api/v1/campaigns    SQLite              │
│  razorpay             GET  /api/v1/campaigns    campaigns           │
│  (HMAC verified)      /{id}/metrics             recovery_sessions   │
│                                                 audit_log           │
│                                                                     │
│  Call Lifecycle API                                                 │
│  POST /api/v1/calls/assign    ← called by var-thon at START_SESSION │
│  POST /api/v1/calls/{id}/end  ← called by var-thon at END_SESSION   │
│                                                                     │
│  Stopping Rules Engine                                              │
│  max 3 attempts | cooldown 24h | refused = stop | recovered = stop  │
└─────────────┬─────────────────────────────────────┬────────────────┘
              │ Razorpay Test APIs                  │ Razorpay Webhooks
              │ POST /v1/payment_links              │ payment.failed
              │ (send link post-call if agreed)     │ payment.captured
              ▼                                     │
     ┌─────────────────┐                            │
     │  Razorpay       │ ◄──────────────────────────┘
     │  Test Mode      │
     └─────────────────┘

Browser tab (demo: manually opened per customer)
        │ WebRTC
        ▼
┌────────────────────┐
│    AetherRTC       │   ← ORIGINAL REPO, UNCHANGED
│  (G.711/PCMU,      │
│   Pion, gRPC       │
│   bridge)          │
└────────┬───────────┘
         │ gateway.proto (session_id flows through)
         ▼
┌────────────────────────────────────────────────────────┐
│            var-thon: Orchestrator-Go                   │
│                                                        │
│  On START_SESSION:                                     │
│    1. Receive session_id from AetherRTC                │
│    2. POST recovery-orchestrator/api/v1/calls/assign   │
│       → get {system_prompt, customer_name, amount}     │
│    3. Send ControlSignal with dynamic system_prompt    │
│       to Inference-Python                              │
│                                                        │
│  On END_SESSION:                                       │
│    1. POST recovery-orchestrator/api/v1/calls/{id}/end │
└────────────────────────────┬───────────────────────────┘
                             │ agent.proto
                             ▼
┌────────────────────────────────────────────────────────┐
│           var-thon: Inference-Python                   │
│                                                        │
│  New profile: recovery_agent.yaml                      │
│  (system prompt comes from Recovery Orchestrator,      │
│   not from file — but the yaml sets defaults)          │
│                                                        │
│  STT: stays pinned to English (0.67s saving kept)      │
│  LLM: Qwen2.5:3b via Ollama                            │
│  TTS: Piper                                            │
│  Conversation history: intact per session              │
└────────────────────────────────────────────────────────┘
```

---

## What Changes, By File

**var-thon — Orchestrator-Go**

The one structural change: `internal/gateway/server.go` (wherever `START_SESSION` is handled, where `NewSession` is called with a profile). After receiving `START_SESSION` from AetherRTC:

```go
// current: load profile from -profile flag at startup
profile := cfg.AgentProfile

// var-thon: call Recovery Orchestrator for per-session context
ctx, err := recoveryClient.AssignSession(sessionID)
if err != nil || ctx == nil {
    // fallback to static profile — session can still proceed
    profile = cfg.AgentProfile
} else {
    profile = buildProfileFromContext(ctx)
}
```

`recoveryClient` is a thin HTTP client (20 lines). `buildProfileFromContext` takes the returned JSON and constructs the `AgentProfile` struct with the dynamic system prompt. Nothing else in Orchestrator-Go changes.

**var-thon — configs/agent_profiles/recovery_agent.yaml**

New file. The system prompt becomes the backbone of the recovery persona. This is the most important thing to get right for the demo quality. Rough structure:

```yaml
name: recovery_agent
agent_name: Priya
voice: en_GB-alba-medium # or whatever Piper voice sounds professional

system_prompt: |
  You are Priya, a professional payment recovery specialist calling on behalf
  of a Razorpay merchant. You are calling {customer_name} regarding an
  outstanding payment of {amount} for {product} due on {due_date}.

  Your objectives, in order:
  1. Confirm you are speaking with {customer_name}.
  2. Briefly explain the outstanding payment.
  3. Offer to resolve it — either payment now (you will arrange a link)
     or a confirmed future date.
  4. If they cannot pay now, get a specific date and confirm it clearly.
  5. If they decline entirely, acknowledge respectfully and close.

  Rules:
  - Never mention the payment more than three times if refused.
  - Never be aggressive, threatening, or pressuring.
  - If they agree: say exactly "I will arrange a payment link for you."
  - If they give a date: repeat the date back to confirm it.
  - Keep the call under 3 minutes.
  - You are calling from India. Speak natural, professional Indian English.
```

The `{customer_name}`, `{amount}` etc. are template-filled by the Recovery Orchestrator before the system prompt string reaches var-thon. It returns a fully rendered string, not a template.

**var-thon — Inference-Python**

No changes required beyond picking up whatever system prompt arrives via `ControlSignal`. Dynamic profiling is already built (from backlog: "system_prompt extracted and passed directly into LLMEngine.generate() via system_override"). This just works.

---

## Recovery Orchestrator — SQLite Schema

Three tables. This is the audit trail the judges will look at.

```sql
CREATE TABLE campaigns (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    total       INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'active',
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE recovery_sessions (
    id                      TEXT PRIMARY KEY,
    campaign_id             TEXT NOT NULL REFERENCES campaigns(id),
    call_session_id         TEXT,                   -- AetherRTC session_id, bound at call start
    customer_name           TEXT NOT NULL,
    outstanding_paise       INTEGER NOT NULL,
    product_name            TEXT NOT NULL,
    due_date                TEXT,
    razorpay_payment_id     TEXT,                   -- from the webhook that triggered recovery
    system_prompt           TEXT NOT NULL,
    status                  TEXT NOT NULL DEFAULT 'pending',
    -- pending | active | recovered | promised | refused | no_answer
    promise_date            TEXT,
    razorpay_link_id        TEXT,                   -- created post-call if agreed
    contact_attempts        INTEGER NOT NULL DEFAULT 0,
    max_contact_attempts    INTEGER NOT NULL DEFAULT 3,
    created_at              DATETIME DEFAULT CURRENT_TIMESTAMP,
    call_started_at         DATETIME,
    call_ended_at           DATETIME,
    recovered_at            DATETIME
);

CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT NOT NULL,
    event_type  TEXT NOT NULL,
    -- webhook_received | session_assigned | call_started | call_ended
    -- payment_link_sent | payment_captured | stopped | promise_logged
    event_data  TEXT,                               -- JSON blob
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

Every state transition writes a row to `audit_log`. The judges can run `SELECT * FROM audit_log WHERE session_id = 'x' ORDER BY created_at` and see exactly what happened and when.

---

## Outcome Detection (Without Phase 2 Tool Calling)

This is the one design decision that needs to be explicit. Without mid-call tool calling, var-thon can't signal recovery intent during the call. Two options for the demo:

**Option A (recommended):** Post-call LLM classification

At `END_SESSION`, var-thon's Python side has the full conversation history in `SessionContext`. Add a single non-streaming LLM call after the last utterance:

```python
classification_prompt = """
Based on the conversation above, the customer's response was:
A) AGREED — they agreed to pay now and want a payment link
B) PROMISED — they committed to pay on a specific date
C) REFUSED — they declined to pay
D) UNCLEAR — no clear resolution

Reply with exactly one word: AGREED, PROMISED, REFUSED, or UNCLEAR.
"""
outcome = llm.generate_sync(conversation_history + classification_prompt)
```

This gets sent to Recovery Orchestrator in the `POST /api/v1/calls/{id}/end` body. No new proto messages needed — happens in Python after the session closes, reported via HTTP.

**Option B:** Manual marking in a simple dashboard. Demo operator clicks outcome after each call. Honest, simple, works.

Start with Option A. If it misclassifies during demo rehearsal, fall back to Option B. Both are defensible to judges.

---

## What the Judges See

The demo has three parts:

**Part 1: Batch loading**

```
POST /api/v1/campaigns
{
  "name": "August Failed EMI Recovery",
  "accounts": [20 synthetic records]
}
→ Campaign created, 20 sessions queued
```

**Part 2: Live call** (30-60 seconds of actual voice conversation)

- Open browser tab
- Recovery agent identifies itself, states the amount, negotiates
- Customer (you, the presenter) responds
- Agent handles agreement/refusal gracefully
- Call ends

**Part 3: Metrics output**

```
Campaign: August Failed EMI Recovery
Total accounts: 20
Contacted: 12
  └─ Recovered: 5  (₹21,400)
  └─ Promised:  4  (₹17,800 pending)
  └─ Refused:   2
  └─ No answer: 1
Remaining: 8 (awaiting contact)
Stopped (max attempts): 1
Payment links sent: 5
Razorpay payment.captured confirmed: 3

Audit trail: audit.db (all events logged with timestamps)
```

That answers the judging bar exactly: batch, measured recovery, stopping rules, audit trail.

---

## Build Order

Given the time constraint:

1. Fork VAR → `var-thon`. Verify it runs end-to-end before touching anything (don't start building on a broken base).
2. `recovery-orchestrator`: HTTP server skeleton, SQLite schema, campaign create + session queue endpoints. Get `/api/v1/calls/assign` working with hardcoded test data.
3. `var-thon` Orchestrator-Go: add the `AssignSession` HTTP call at `START_SESSION`. Test it calls assign and gets a system prompt back.
4. `recovery_agent.yaml`: write the system prompt. Run 3-4 test calls and iterate until the agent sounds right.
5. Razorpay test-mode integration: webhook consumer (simulate `payment.failed` events to populate the queue) + `POST /v1/payment_links` after call if outcome is AGREED.
6. Post-call classification (Option A above).
7. Metrics endpoint + output formatting.
8. End-to-end run through a full 20-account synthetic batch. Fix anything that breaks.
9. Demo rehearsal. Time the live call section. Make sure the agent's opening line is tight.

What's the hackathon submission deadline? That determines how aggressive the schedule needs to be and whether Option A classification is worth the build time.
