# Vasuli

> **वसूली** _(vasuli)_: recovery, in the sense an Indian lender means it. Getting the money back.

A voice agent that holds a real conversation with someone about an overdue
payment, then does something about how it went. If they agree to pay, a
Razorpay payment link is created before the call is even finished being
written to the database. If they ask for time, the system waits and calls
back. If they dispute the debt, it stops calling them, permanently, and
the account goes to a human.

The whole voice pipeline runs on one laptop. Speech recognition, the
language model, speech synthesis, and the WebRTC transport are all local,
on an RTX 3050 with 4GB of VRAM. The only outbound network call the system
makes during a recovery is to Razorpay, to create the link.

Submission for the Razorpay AI Buildathon 2026, Track 3: AI Revenue Recovery.

---

## The problem

A failed EMI payment is worth chasing. Ten thousand of them are worth
chasing too, and that is where it falls apart. Human agents cost money and
have bad days. SMS and email get ignored, and neither can answer "can I pay
on the fifth instead."

So the recoverable money leaks quietly, one dropped follow-up at a time.

## What actually happens on a call

The system picks who to call from a queue it maintains itself. Nobody
selects the customer.

Priya, the agent, opens the call knowing the name, the amount, the product,
and the due date. She confirms she is speaking to the right person, states
what is owed, and asks for payment. Then she listens.

Four things can come out of that conversation, and each one means something
different to the queue:

| Outcome | What the system does |
| ------- | -------------------- |
| Agrees to pay | Creates a real Razorpay payment link, follows up in 24 hours if it goes unpaid |
| Names a date | Records it and waits until that date |
| Reaches no conclusion | Tries again after 24 hours |
| Disputes the debt | Stops calling. Permanently. The account is escalated to a human |

Nobody marks any of this by hand. The outcome is classified from the
conversation transcript after the call ends, and the queue reschedules
itself.

Three attempts is the ceiling. After that the account stops being called
regardless of outcome, because a recovery workflow that never terminates is
a harassment workflow.

Every step is written to an append-only audit log: who was queued, when
they were called, what they said, what was created, when payment was
confirmed. That log is the point. A recovery system nobody can audit is not
one a merchant can use.

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

## What this is not

Worth saying before you find it yourself.

**The customer joins the call. The system does not dial them.** Real
outbound recovery means placing a call over PSTN, and that means a
telephony provider, a number, a per-minute bill, and DND compliance. None
of that would change anything about the recovery logic, so it was left out
and a browser session used instead.

The distinction that matters is narrower than it sounds. The *transport* is
inbound. The *workflow* is outbound throughout: the system picks who to
contact from its own queue, the agent speaks first, drives the agenda, and
decides what happens next. Swapping AetherRTC for a SIP gateway would touch
nothing downstream, because `gateway.proto` deliberately carries no
knowledge of agents, transcripts, or customers. It moves audio and a
session id.

What genuinely would need building for real outbound: assignment inverts
from pull to push, outcomes like `no_answer` and `busy` start existing,
something has to decide when and how fast to dial, and TRAI's DND rules
apply.

**Outcome classification is the weakest component**, and it is the one that
scales with model size rather than with engineering. A 3B model running in
4GB of VRAM classifies a four-word answer well and struggles with a whole
conversation. Measured behaviour: a call ending "thank you" classifies
correctly three times out of three, while the same call ending "no thanks,
bye" flips to REFUSED, because the model reads that "no" as refusing the
payment rather than declining further help. The system degrades safely, as
no payment link is created on an unclear call, but the limitation is real
and it is documented in `docs/build-log.md` rather than hidden.

**There is no authentication on any endpoint.** It binds to localhost and
is meant to. Anyone who can reach port 8090 can create a campaign or record
an outcome. Adding auth is not interesting work and it was not done.

**One known distributed-systems hole**, described properly in the build
log: if the Razorpay payment-link call times out *after* Razorpay has
already created the link, nothing records it and a retry creates a second
one. The fix is an idempotency key on the request. It was not built,
because the schedule ran out before the care that change deserves.

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
# Terminal 1, Inference engine
cd var-thon/services/inference-py
uv run python main.py

# Terminal 2, var-thon Orchestrator-Go (gateway mode)
cd var-thon/services/orchestrator-go
go run ./cmd/gateway-server \
  -profile recovery_agent \
  -port :50052 \
  -inference localhost:50051 \
  -recovery http://localhost:8090

# Terminal 3, Recovery Orchestrator
cd recovery-orchestrator
go run ./cmd/main.go \
  -port :8090 \
  -db ./vasuli.db \
  -razorpay-key-id YOUR_KEY_ID \
  -razorpay-key-secret YOUR_KEY_SECRET

# Terminal 4, AetherRTC (from its own repo)
go run ./cmd/gateway/main.go

# Browser, open index.html from AetherRTC repo, click Start Call
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

## A note on how this was reviewed

Parts of this repository were written with an AI coding agent, and I would
rather say so than have someone work it out. The instructions I gave it are
in [`AGENTS.md`](AGENTS.md), including the standard it was held to and the
things it was told never to do. I think how somebody directs a tool says
more about them than whether they used one.

What I would ask, if the choice exists, is that a person reads this rather
than a model. Not because I think the code would score worse. Because the
parts I am actually proud of are judgement calls that only make sense in
context: why the follow-up window is 24 hours and not a tunable, why
recording an outcome twice had to become impossible rather than merely
unlikely, why a payment link resolves by link id and not by payment id.
Those are in the code and in [`docs/build-log.md`](docs/build-log.md), and
they read as ordinary lines until you know what they are load-bearing for.

If that request turns out to be inconvenient, ignore it. The repository
stands on its own either way.

---

## Docs

- [Setup](docs/setup.md), Prerequisites, credentials, and running the services
- [Architecture](docs/architecture.md), Full technical design and data flow
- [API](docs/api.md), Endpoint reference and webhook handling
- [Build log](docs/build-log.md), What broke during the build and how it was found
- [Roadmap](docs/roadmap.md), Milestone plan and build order

---

## License

Apache 2.0
