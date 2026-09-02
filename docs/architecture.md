# Vasuli, Architecture

## Overview

Vasuli is a three-service distributed system. Each service has a single, well-defined responsibility and communicates with adjacent services over explicit contracts. No service reaches across its boundary into another service's domain.

```
Recovery Orchestrator   ←→   Razorpay Test-Mode APIs
        ↕ HTTP (REST)
var-thon Orchestrator-Go
        ↕ gateway.proto (gRPC, bidirectional stream)
AetherRTC Edge Gateway
        ↕ WebRTC / G.711 PCMU
Browser (Customer)
```

And separately, the internal voice pipeline:

```
var-thon Orchestrator-Go
        ↕ agent.proto (gRPC, bidirectional stream)
var-thon Inference-Python
        (Silero VAD → Faster-Whisper → Qwen2.5:3b → Piper TTS)
```

---

## Service Responsibilities

### Recovery Orchestrator

The only service that knows about Razorpay, customers, campaigns, and business logic.

**Owns:**

- Campaign lifecycle: create, track, close
- Recovery session queue: which customer to call next
- Stopping rules: max attempts, cooldown, refused/recovered flags
- Razorpay webhook consumption: `payment.failed`, `payment.captured`
- Razorpay outbound calls: create payment links post-call
- Audit trail: every event logged to SQLite with timestamp and payload

**Does not own:**

- Voice conversation logic
- Audio transport
- AI model selection or inference

**Exposes (HTTP):**

| Method | Path                            | Called by     | Purpose                                     |
| ------ | ------------------------------- | ------------- | ------------------------------------------- |
| POST   | `/webhooks/razorpay`            | Razorpay      | Ingest `payment.failed`, `payment.captured` |
| POST   | `/api/v1/campaigns`             | Operator      | Create campaign, bulk-load accounts         |
| GET    | `/api/v1/campaigns/:id`         | Operator      | Campaign status                             |
| GET    | `/api/v1/campaigns/:id/metrics` | Operator/Demo | Recovery metrics                            |
| POST   | `/api/v1/calls/assign`          | var-thon      | Pop next pending session, return context    |
| POST   | `/api/v1/calls/:id/end`         | var-thon      | Record call end + outcome                   |

---

### var-thon (Voice Agent Runtime)

Adapted from VAR. Manages the voice session lifecycle and bridges audio between AetherRTC and the inference engine.

**Orchestrator-Go owns:**

- Session state machine: `CREATED → CONNECTING → ACTIVE → PROCESSING → RESPONDING → TERMINATED`
- Protocol translation: `gateway.proto` ↔ `agent.proto`
- Per-session context fetch from Recovery Orchestrator (new in var-thon)
- Dynamic system prompt injection via `ControlSignal`

**Inference-Python owns:**

- Voice Activity Detection: Silero VAD (ONNX), four-state debounce machine
- Speech-to-Text: Faster-Whisper, CPU, int8, English pinned
- Language Model: Qwen2.5:3b via Ollama, streaming with sentence-boundary chunking
- Text-to-Speech: Piper TTS, CPU
- Post-call outcome classification: LLM inference on full conversation history

**Does not own:**

- WebRTC transport
- Customer data or business rules
- Razorpay API calls

---

### AetherRTC

Unchanged from its original repository. The edge media gateway.

**Owns:**

- WebRTC termination: SDP negotiation, ICE traversal, DTLS handshake, SRTP
- G.711 µ-law decode (inbound RTP → PCM) and encode (PCM → outbound RTP)
- gRPC bridge to var-thon over `gateway.proto`

**Does not own:**

- Any concept of what an utterance is
- Any AI model or inference logic
- Any business logic or customer data

---

## Data Flow: A Recovery Call

This is the complete sequence from call start to audit record.

```
1. Recovery Orchestrator has a queue of pending recovery sessions.
   Each session has: customer_name, outstanding_paise, product_name,
   due_date, and a fully-rendered system prompt string.

2. Demo operator opens a browser tab → AetherRTC's index.html.

3. Browser sends SDP offer over WebSocket to AetherRTC signaling.
   AetherRTC generates a session_id (UUID), creates a PeerSession,
   completes WebRTC negotiation, returns SDP answer.

4. Browser microphone begins sending G.711 audio over RTP.
   AetherRTC decodes µ-law → PCM, pushes to PCMInboundChan.

5. AetherRTC bridge opens gateway.proto StreamAudio to var-thon:50052.
   Sends: GatewayControl{START_SESSION, source_sample_rate: 8000}
   with the session_id generated in step 3.

6. var-thon Orchestrator-Go receives START_SESSION.
   Makes HTTP call: POST recovery-orchestrator:8090/api/v1/calls/assign
   Body: {"call_session_id": "<uuid>"}

7. Recovery Orchestrator:
   - Checks the pending queue (FIFO, respects stopping rules)
   - Pops the next eligible session
   - Binds call_session_id to that recovery_session
   - Updates status: pending → active
   - Writes audit_log: event_type=call_started
   - Returns: {system_prompt, customer_name, outstanding_amount_paise, ...}

8. var-thon Orchestrator-Go:
   - Opens agent.proto StreamEvents to Inference-Python:50051
   - Sends ControlSignal{START_SESSION, profile: {system_prompt, agent_name: "Priya"}}

9. Inference-Python initializes fresh per-session state:
   - VADDetector (Silero ONNX, four-state debounce)
   - AudioPreprocessor (8kHz → 16kHz resampling)
   - Empty conversation history

10. Audio flows:
    Browser mic → AetherRTC → gateway.proto → var-thon → agent.proto → Inference-Python

11. Inference-Python processes audio continuously:
    - VAD detects speech boundary (SILENCE→SPEECH_STARTING→SPEECH→SPEECH_ENDING)
    - On END_OF_UTTERANCE: passes utterance frames to STT
    - STT (Faster-Whisper): transcript in ~0.98s
    - LLM (Qwen2.5:3b): streaming response, TTFT ~0.42s
    - Sentence-boundary chunking: TTS begins on first sentence while LLM generates
    - TTS (Piper): audio bytes returned per sentence
    - Audio bytes flow back: agent.proto → var-thon → gateway.proto → AetherRTC → RTP → browser

12. Call ends (customer closes tab or AetherRTC disconnects).
    Inference-Python receives stream EOF.

13. BEFORE signaling done: Inference-Python runs post-call classification.
    Single blocking LLM call on full conversation history:
    "Reply with one word: AGREED, PROMISED, REFUSED, or UNCLEAR"
    Result: outcome string.

14. var-thon Orchestrator-Go detects session end (DoneChan closes).
    Makes HTTP call: POST recovery-orchestrator:8090/api/v1/calls/<uuid>/end
    Body: {"outcome": "AGREED", "promise_date": null}

15. Recovery Orchestrator:
    - Updates recovery_sessions.status based on outcome
    - Writes audit_log: event_type=call_ended, event_data={outcome}
    - If AGREED: calls Razorpay POST /v1/payment_links
      Stores razorpay_link_id in recovery_sessions
      Writes audit_log: event_type=payment_link_sent
    - If PROMISED: stores promise_date
      Writes audit_log: event_type=promise_logged
    - If REFUSED: sets status=refused (stopping rules will prevent future contact)

16. Later: Razorpay fires payment.captured webhook.
    Recovery Orchestrator verifies HMAC signature.
    Updates recovery_sessions.status = 'recovered', recovered_at = NOW()
    Writes audit_log: event_type=payment_captured
    Updates campaign metrics.
```

---

## Proto Contracts

### agent.proto, Orchestrator-Go ↔ Inference-Python

The AI-aware contract. Carries agent profiles, transcripts, and control signals.

```protobuf
service VoiceAgent {
  rpc StreamEvents(stream Event) returns (stream Event);
}

message Event {
  string session_id = 1;
  oneof payload {
    AudioChunk   audio      = 2;
    Transcript   transcript = 3;
    ControlSignal control   = 4;
  }
}

message ControlSignal {
  enum SignalType {
    START_SESSION    = 0;
    END_SESSION      = 1;
    BARGE_IN         = 2;
    END_OF_UTTERANCE = 3;
  }
  SignalType   type               = 1;
  AgentProfile profile            = 2;
  int32        source_sample_rate = 3;
}

message AgentProfile {
  string agent_name    = 1;
  string system_prompt = 2;
}
```

### gateway.proto, AetherRTC ↔ Orchestrator-Go

Transport-only contract. No AI concepts. Carries audio bytes and connection metadata.

```protobuf
service Gateway {
  rpc StreamAudio(stream GatewayEvent) returns (stream GatewayEvent);
}

message GatewayEvent {
  string session_id = 1;
  oneof payload {
    AudioChunk     audio   = 2;
    GatewayControl control = 3;
  }
}

message GatewayControl {
  enum SignalType { START_SESSION = 0; END_SESSION = 1; }
  SignalType type               = 1;
  int32      source_sample_rate = 2;
}
```

---

## Database Schema

Three tables in a single SQLite file (`vasuli.db`).

```sql
-- A recovery campaign is a batch of accounts to contact.
CREATE TABLE campaigns (
    id          TEXT PRIMARY KEY,   -- UUID
    name        TEXT NOT NULL,
    total       INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'active',  -- active | completed | paused
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- One row per customer per recovery attempt.
-- status lifecycle: pending → active → link_sent | promised | unclear | refused | failed
--                   link_sent → recovered (on payment confirmation)
--
-- There is no no_answer status. The transport is a browser session the
-- customer joins, so a call is never unanswered; a metric that can only
-- ever be zero is worse than no metric.
CREATE TABLE recovery_sessions (
    id                   TEXT PRIMARY KEY,     -- UUID (internal recovery ID)
    campaign_id          TEXT NOT NULL REFERENCES campaigns(id),
    call_session_id      TEXT,                 -- AetherRTC session_id, set at call start
    customer_name        TEXT NOT NULL,
    outstanding_paise    INTEGER NOT NULL,     -- amount in paise (₹ × 100)
    product_name         TEXT NOT NULL,
    due_date             TEXT,                 -- ISO date string
    razorpay_payment_id  TEXT,                 -- failed payment ID from webhook
    system_prompt        TEXT NOT NULL,        -- fully-rendered, injected at call start
    status               TEXT NOT NULL DEFAULT 'pending',
    promise_date         TEXT,                 -- set if outcome = PROMISED
    razorpay_link_id     TEXT,                 -- set after payment link created
    contact_attempts     INTEGER NOT NULL DEFAULT 0,
    max_contact_attempts INTEGER NOT NULL DEFAULT 3,
    created_at           DATETIME DEFAULT CURRENT_TIMESTAMP,
    call_started_at      DATETIME,
    call_ended_at        DATETIME,
    recovered_at         DATETIME
);

-- Append-only event log. Every significant state change writes here.
-- This is the primary audit trail.
CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT NOT NULL,        -- references recovery_sessions.id
    event_type  TEXT NOT NULL,
    -- webhook_received | session_assigned | call_started | utterance_transcribed
    -- call_ended | outcome_classified | payment_link_sent | payment_captured
    -- stopped_max_attempts | stopped_refused | promise_logged
    event_data  TEXT,                 -- JSON payload, varies by event_type
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_recovery_sessions_status ON recovery_sessions(status);
CREATE INDEX idx_recovery_sessions_campaign ON recovery_sessions(campaign_id);
CREATE INDEX idx_audit_log_session ON audit_log(session_id);
```

---

## Scheduling and Stopping Rules

A call is never "finished with" a customer, it decides **when, if ever, to
call them again**. That decision is written to `next_eligible_at` at the
moment the outcome is recorded, and one predicate governs every assignment:

```sql
next_eligible_at IS NOT NULL          -- NULL means never again
AND next_eligible_at <= CURRENT_TIMESTAMP
AND contact_attempts < max_contact_attempts
AND status IN ('pending', 'unclear', 'link_sent', 'promised')
```

`store.eligibleNow` is that predicate, and both the assignment query and the
queue view read from it. So the system cannot report someone as due while
the assigner skips them.

| Outcome | Next contact | Reasoning |
| ------- | ------------ | --------- |
| Never called | immediately |, |
| `UNCLEAR` | +24h | no verdict reached; worth another attempt |
| `AGREED`, link unpaid | +24h | the link has expired by then, so a fresh one is needed |
| `PROMISED` | the promised date, else +24h | the customer set the terms |
| `REFUSED` | **never** | escalated to a human agent |
| Payment confirmed | **never** | `next_eligible_at` is cleared |
| Attempts exhausted | **never** | the workflow is bounded |
| Campaign paused | skipped | `campaign.status != 'active'` |

**The 24-hour cooldown is not an arbitrary constant.** It equals
`razorpay.linkValidity`, the lifetime of a generated payment link. The
follow-up therefore lands exactly when the old link dies, so calling back
is not nagging someone who already holds a working link, it exists to
issue a new one. Changing one number without the other breaks that.

**`refused` is a handoff, not an abandonment.** A customer disputing the
debt has moved past what an automated agent should handle, so Vasuli stops
permanently and the account belongs to a human. Metrics report this as
`escalated_to_human` rather than `refused`.

Two independent things must both hold for a customer to be called, which is
deliberate: `next_eligible_at` is edited by hand during demos, and the
status check ensures a stray `UPDATE` cannot revive a customer the system
promised never to contact again.

---

## Outcome Classification

Inference-Python runs a single non-streaming LLM inference after the call ends, on the full `conversation_history` from the session:

```python
classification_messages = [
    {"role": "system", "content": "You classify payment recovery call outcomes. Reply with exactly one word."},
    *ctx.conversation_history,
    {
        "role": "user",
        "content": (
            "Based on the conversation above, what was the customer's final outcome?\n"
            "AGREED. Customer agreed to pay now and wants a payment link\n"
            "PROMISED. Customer committed to a specific future payment date\n"
            "REFUSED, customer explicitly declined to pay\n"
            "UNCLEAR, no clear resolution reached\n\n"
            "Reply with exactly one word."
        )
    }
]
```

The result is sent to Recovery Orchestrator in the `POST /api/v1/calls/:id/end` request body. The Recovery Orchestrator makes no inference itself, it receives and stores the outcome.

---

## Key Design Decisions

**Queue-based context injection, not pre-registered session IDs**

AetherRTC generates session IDs. Rather than requiring the Recovery Orchestrator to pre-know those IDs (which would require AetherRTC modification), var-thon calls the Recovery Orchestrator immediately after receiving `START_SESSION` from AetherRTC. The Recovery Orchestrator assigns the next pending customer to that call on demand. AetherRTC remains unchanged.

**Dynamic system prompts, not per-customer YAML files**

Each customer needs a different system prompt (different name, amount, product). The Recovery Orchestrator renders the template at campaign-load time and stores the fully-rendered string in `recovery_sessions.system_prompt`. var-thon receives a plain string, it never does template substitution itself.

**Post-call LLM classification, not mid-call tool calling**

Outcome detection (AGREED/PROMISED/REFUSED) happens after the call ends, using the full conversation history. This avoids the complexity of mid-call tool invocation (Phase 2 in VAR's roadmap) while still giving a high-quality, context-aware classification. If the classification is wrong on a specific call, it can be overridden manually without affecting the system design.

**Fallback to static profile when Recovery Orchestrator is unavailable**

If the `POST /api/v1/calls/assign` call fails or returns no pending session (empty queue), var-thon falls back to the static `recovery_agent.yaml` profile. The call proceeds. This prevents a Recovery Orchestrator crash from breaking the voice pipeline.
