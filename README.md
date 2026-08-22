# Vasuli — AI-Powered Payment Recovery via Voice

> **वसूली** _(vasuli)_: the act of collection or recovery in Indian finance and banking.

Vasuli is a distributed, real-time voice agent system that autonomously contacts customers with failed or overdue payments, conducts structured recovery conversations, and executes bounded recovery workflows — measured across a batch, with a full audit trail.

Built on a custom-engineered voice infrastructure stack (Go + Python, gRPC, WebRTC) and integrated with Razorpay's test-mode APIs, Vasuli closes the loop from `payment.failed` webhook through AI voice negotiation to `payment.captured` confirmation.

**Razorpay Buildathon 2026 — Track 3: AI Revenue Recovery**

---

## The Problem

In Indian B2C and BFSI, failed payments create a manual coordination bottleneck that does not scale:

- EMI defaults, subscription lapses, and overdue invoices require outbound customer contact
- Human recovery agents are expensive, inconsistent in quality, and cannot handle volume
- SMS and email nudges have low engagement rates and no two-way negotiation capability
- The window between `payment.failed` and customer contact is filled with latency and dropped cases

The result: recoverable revenue silently leaks. Merchants on Razorpay have no automated, conversational path from a failed payment event to an actual recovered transaction.

Vasuli is that path.

---

## What Vasuli Does

```
Razorpay payment.failed webhook
        ↓
Recovery Orchestrator creates a session for the customer
        ↓
Customer joins a recovery call via browser link
        ↓
AI voice agent (Priya) conducts a professional recovery conversation
        ↓
Customer agrees → payment link dispatched via Razorpay API
Customer promises → promise-to-pay date logged with follow-up scheduled
Customer refuses → stopping rules enforced, no further contact
        ↓
payment.captured webhook confirms recovery
        ↓
Batch metrics updated: recovered amount, recovery rate, audit trail
```

A single campaign can queue 20–100 failed payment accounts. Every contact attempt, conversation outcome, and payment action is logged with timestamps. Stopping rules prevent harassment. The audit trail is queryable.

---

## System Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                       RECOVERY ORCHESTRATOR                         │
│                        recovery-orchestrator/                       │
│                                                                     │
│  Webhook Consumer (HMAC-verified)   Campaign/Batch Manager          │
│  POST /webhooks/razorpay            POST /api/v1/campaigns          │
│                                     GET  /api/v1/campaigns/:id      │
│                                                                     │
│  Call Lifecycle API                 Stopping Rules Engine           │
│  POST /api/v1/calls/assign          max_attempts=3                  │
│  POST /api/v1/calls/:id/end         cooldown=24h                    │
│                                     refused → stop                  │
│  Audit Store (SQLite)               recovered → stop                │
│  campaigns | recovery_sessions | audit_log                          │
└──────┬──────────────────────────────────────┬───────────────────────┘
       │ POST /v1/payment_links               │ payment.captured
       │ Razorpay Test-Mode API               │ Razorpay Webhooks
       ▼                                      │
  ┌─────────────┐                             │
  │  Razorpay   │ ◄───────────────────────────┘
  │  Test Mode  │
  └─────────────┘

Browser (customer opens recovery link)
       │ WebRTC / G.711 PCMU
       ▼
┌─────────────────────────────────────┐
│            AetherRTC                │  ← Edge Media Gateway
│   WebRTC termination, G.711 codec   │    (separate repo, unchanged)
│   gRPC bridge to orchestrator       │
└──────────────┬──────────────────────┘
               │ gateway.proto (bidirectional gRPC stream)
               ▼
┌─────────────────────────────────────────────────────────┐
│              var-thon: Orchestrator-Go                  │
│                     var-thon/                           │
│                                                         │
│  On START_SESSION:                                      │
│    → POST recovery-orchestrator/api/v1/calls/assign     │
│    → Receive {system_prompt, customer_name, amount}     │
│    → Forward dynamic ControlSignal to Inference-Python  │
│                                                         │
│  On END_SESSION:                                        │
│    → POST recovery-orchestrator/api/v1/calls/:id/end    │
│    → Include {outcome: AGREED|PROMISED|REFUSED|UNCLEAR} │
└──────────────────────────┬──────────────────────────────┘
                           │ agent.proto (bidirectional gRPC stream)
                           ▼
┌─────────────────────────────────────────────────────────┐
│            var-thon: Inference-Python                   │
│                                                         │
│  Silero VAD  →  Faster-Whisper STT  →  Qwen2.5:3b LLM  │
│         →  Piper TTS  →  Post-call outcome classifier   │
│                                                         │
│  Dynamic system prompt: injected per session            │
│  Conversation history: maintained across all utterances │
│  Post-call: LLM classifies outcome from full history    │
└─────────────────────────────────────────────────────────┘
```

---

## Repository Structure

```
vasuli/
├── README.md                          ← You are here
├── var-thon/                          ← Voice Agent Runtime (adapted)
│   ├── proto/
│   │   ├── agent.proto                Agent ↔ Inference gRPC contract
│   │   └── gateway.proto              AetherRTC ↔ Orchestrator contract
│   ├── services/
│   │   ├── orchestrator-go/           Session lifecycle, gateway bridge
│   │   │   ├── internal/
│   │   │   │   ├── gateway/server.go  ← Recovery context fetch (new)
│   │   │   │   └── recovery/client.go ← HTTP client to Recovery Orchestrator (new)
│   │   │   └── configs/agent_profiles/
│   │   │       ├── receptionist.yaml  (existing, unchanged)
│   │   │       └── recovery_agent.yaml ← (new)
│   │   └── inference-py/              VAD → STT → LLM → TTS pipeline
│   └── docs/                          VAR architecture documentation
│
└── recovery-orchestrator/             ← New service (hackathon-specific)
    ├── cmd/main.go                    HTTP server entry point
    ├── internal/
    │   ├── api/                       HTTP handlers and router
    │   ├── campaign/                  Batch management and stopping rules
    │   ├── razorpay/                  Webhook consumer and API client
    │   └── store/                     SQLite schema and query layer
    └── go.mod
```

---

## Tech Stack

| Component                    | Technology                  | Role                              |
| ---------------------------- | --------------------------- | --------------------------------- |
| Voice pipeline orchestration | Go, gRPC                    | Session lifecycle, protocol relay |
| AI inference                 | Python                      | VAD, STT, LLM, TTS                |
| Voice Activity Detection     | Silero VAD (ONNX)           | Speech boundary detection         |
| Speech-to-Text               | Faster-Whisper (CPU, int8)  | Transcription                     |
| Language Model               | Qwen2.5:3b via Ollama (GPU) | Conversation reasoning            |
| Text-to-Speech               | Piper TTS (CPU)             | Voice synthesis                   |
| WebRTC transport             | AetherRTC (Pion, Go)        | Browser ↔ backend audio           |
| Recovery orchestration       | Go, net/http                | Campaign management, API bridge   |
| Audit store                  | SQLite                      | Session records, audit trail      |
| Payment APIs                 | Razorpay test-mode          | Webhooks, payment links           |

---

## Measured Performance (var-thon baseline, warm model)

Hardware: RTX 3050 Mobile 4GB VRAM, Ryzen 5 6600H, 16GB DDR5.

| Stage      | Metric                                 | Value       |
| ---------- | -------------------------------------- | ----------- |
| STT        | Transcription latency (English pinned) | ~0.98s      |
| LLM        | Time to first token (TTFT)             | ~0.42s      |
| TTS        | First sentence synthesis               | ~0.035s     |
| TTS        | Subsequent sentences (warm)            | ~0.05–0.20s |
| End-to-end | VAD boundary → first audio byte        | ~1.1s       |

The pipeline runs entirely on local hardware. No external API calls during inference.

---

## How to Run

### Prerequisites

- Go 1.21+
- Python 3.11, uv
- Ollama with `qwen2.5:3b` pulled
- Piper ONNX model at `var-thon/services/inference-py/models/en_US-lessac-medium.onnx`
- AetherRTC running (see [AetherRTC repo](https://github.com/Adityarya11/aetherRTC))
- Razorpay test-mode API keys (Key ID + Key Secret)

### Start order

```bash
# Terminal 1 — Inference engine
cd var-thon/services/inference-py
uv run python main.py

# Terminal 2 — var-thon Orchestrator-Go (gateway mode)
cd var-thon/services/orchestrator-go
go run ./cmd/gateway-server \
  -profile recovery_agent \
  -port :50052 \
  -inference localhost:50051 \
  -recovery http://localhost:8090

# Terminal 3 — Recovery Orchestrator
cd recovery-orchestrator
go run ./cmd/main.go \
  -port :8090 \
  -db ./vasuli.db \
  -razorpay-key-id YOUR_KEY_ID \
  -razorpay-key-secret YOUR_KEY_SECRET

# Terminal 4 — AetherRTC (from its own repo)
go run ./cmd/gateway/main.go

# Browser — open index.html from AetherRTC repo, click Start Call
```

### Load a recovery campaign

```bash
curl -X POST http://localhost:8090/api/v1/campaigns \
  -H "Content-Type: application/json" \
  -d @recovery-orchestrator/testdata/sample_campaign.json
```

---

## What the Judges See

**1. Batch loaded:**

```
POST /api/v1/campaigns → 20 synthetic failed payment accounts queued
```

**2. Live voice recovery call (45–60 seconds):**

- Open browser tab → Priya (the AI agent) answers
- Priya identifies herself, states the outstanding amount
- Customer (demo presenter) negotiates
- Priya handles the path gracefully: agreement, promise, or refusal
- Call ends

**3. Metrics output:**

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
Razorpay payment.captured:   3   ₹12,600
────────────────────────────────────────
Audit trail: vasuli.db
```

**4. Audit trail:**

```sql
SELECT event_type, event_data, created_at
FROM audit_log
WHERE session_id = 'session_xyz'
ORDER BY created_at;
```

Every event logged: webhook received → session assigned → call started → utterances → call ended → outcome classified → payment link sent → payment captured.

---

## Docs

- [Architecture](docs/architecture.md) — Full technical design and data flow
- [Roadmap](docs/roadmap.md) — Milestone plan and build order
- [Razorpay Integration](docs/razorpay-integration.md) — API reference and webhook handling
- [Demo Guide](docs/demo.md) — Step-by-step demo walkthrough

---

## License

Apache 2.0
