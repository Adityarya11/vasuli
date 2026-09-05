# AGENTS.md

Instructions for any coding agent working in this repository. This is the
authoritative file and it is deliberately tool-agnostic: `AGENTS.md` is the
convention most agent tooling now reads, and this project has been built
with more than one of them.

---

## 1. What this repository contains

Two things that were built at different times and are kept separate on
purpose.

**`var-thon/`** is a self-hosted, real-time voice agent runtime. It predates
this project by about three months and is the substrate everything else sits
on. Three services:

```
Browser <--WebRTC--> AetherRTC <--gRPC--> Orchestrator-Go <--gRPC--> Inference-Python
         (Pion)               (gateway.proto)            (agent.proto)
```

- **AetherRTC** lives in a separate repository. Go and Pion. It terminates
  WebRTC, forces the G.711/PCMU telephony codec rather than Opus (which
  avoids a CGO dependency and keeps the audio path shaped like a real phone
  call), decodes to PCM, and bridges to the orchestrator. It knows nothing
  about AI, utterances, or agent personas. Treat it as frozen: changing it
  is a last resort, not a first instinct.
- **Orchestrator-Go** (`var-thon/services/orchestrator-go`) is the control
  plane. It owns session lifecycle through a strict state machine, bridges
  the two gRPC contracts, and never performs inference.
- **Inference-Python** (`var-thon/services/inference-py`) is the data plane.
  Silero VAD, Faster-Whisper, Qwen2.5 3B via Ollama, Piper TTS. It never
  manages call lifecycle.

Agent personality is entirely YAML, in `var-thon/configs/agent_profiles/`.
The same pipeline runs a dental receptionist or a payment recovery agent
without a line of orchestration or inference code changing.

**`recovery-orchestrator/`** is the payment recovery service, built for the
Razorpay AI Buildathon. Go, standard library only, SQLite through a pure Go
driver. It owns campaigns, the call queue, the stopping rules, follow-up
scheduling, the Razorpay integration, and an append-only audit log. It is
complete and working: campaign load through spoken call, outcome
classification, payment link, signed webhook, confirmed recovery.

The two connect at exactly one point. Orchestrator-Go's gateway asks the
recovery orchestrator for a per-call system prompt and reports the outcome
back over HTTP. That is the entire integration surface.

---

## 2. Non-negotiable rules

1. Never perform GitHub write operations (push, PR, merge, API or CLI
   writes) against `github.com/Adityarya11` repositories. Reads are fine.
   If a task appears to need a write, stop and explain what needs doing.
2. Never hand-edit generated files (`generated/`, `*_pb2*.py`). Edit the
   `.proto` and regenerate. Regenerate per repository; VAR and AetherRTC
   deliberately do not share bindings.
3. Do not push AI-layer concepts (agent profiles, transcripts, utterances,
   system prompts) into AetherRTC. That knowledge belongs in
   Orchestrator-Go. See `var-thon/docs/Three_Tier_Architecture.md` §3.2.
4. Do not embed domain logic in the runtime. A `PropertyLookupModule` or a
   `RecoveryOrchestratorHook` inside the orchestrator or the inference
   engine is an architectural smell. Push back, or isolate it behind
   configuration.
5. Production quality only, including throwaway harnesses. No emojis. No
   placeholder or directional comments. Comments earn their place by
   explaining a non-obvious *why*, never by restating what the code says.
6. Match the surrounding idiom exactly: the Go side's
   `logInfo`/`logWarn`/`logFatal` pattern, Python's per-module
   `logging.getLogger("InferenceEngine.X")`, the transition-map plus mutex
   shape of the state machine. Do not introduce a competing style.
7. Run `gofmt` before finishing. The repository normalizes line endings to
   LF through `.gitattributes`; do not reintroduce CRLF.

---

## 3. How to work here

Act as a senior engineer doing final review before something ships, not as
an autocomplete. Explain the reasoning behind a change, name the risks, and
prefer walking through a fix to silently applying it.

Correctness beats agreeableness. When a stated understanding is wrong, about
concurrency, a protocol detail, an architectural claim, ask what is actually
meant and then correct it precisely. Quietly working around a wrong premise
is the failure mode here, not politeness.

Be economical. No speculative alternative implementations, no restating
unchanged files to show one changed line, no padding.

Design records already exist and remain authoritative. Read before
re-deriving:

| File | Covers |
| --- | --- |
| `docs/architecture.md` | Recovery orchestrator design and data flow |
| `docs/api.md` | Endpoint and webhook reference |
| `docs/build-log.md` | What broke during the build and how it was found |
| `var-thon/docs/HLD.md`, `LLD.md` | Runtime high and low level design |
| `var-thon/docs/Three_Tier_Architecture.md` | Why three tiers; why gRPC wire roles say nothing about authority |
| `var-thon/docs/true_duplex.md` | Threading model, why `sync.Once` guards `DoneChan` |
| `var-thon/docs/vad.md` | Silero integration, the four-state debounce, RNN state lifetime |
| `var-thon/docs/backlog.md` | Runtime status; the most accurate record of what is done |

If a proposal contradicts one of these, flag the conflict rather than
silently overriding it.

---

## 4. Hardware constraints that shape the design

Everything runs on an RTX 3050 Mobile with 4GB of VRAM, a Ryzen 5 6600H,
and 16GB of DDR5.

| Stage | Component | Device | Why |
| --- | --- | --- | --- |
| VAD | Silero (ONNX) | CPU | negligible VRAM cost |
| STT | Faster-Whisper (int8) | CPU | six cores handle it near real time |
| LLM | Qwen2.5 3B via Ollama | GPU | ~2.2GB, leaves headroom on a 4GB card |
| TTS | Piper | CPU | near-zero latency, no VRAM |

This split is a decision, not a placeholder. Moving STT or TTS onto the GPU,
or swapping in a larger model, has to be argued against the 4GB ceiling
explicitly.

---

## 5. Decisions already made

- **gRPC rather than REST** on both hops. Bidirectional streaming over
  HTTP/2 is a requirement for real-time audio; REST would force batching.
- **Go for orchestration, Python for inference.** Goroutines and channels
  fit concurrent I/O; the entire speech and model ecosystem is Python.
- **A single `Event` / `GatewayEvent` message with a `oneof` payload** on
  both hops, so the protocol can grow without breaking transport.
- **Session authority always lives in Orchestrator-Go**, whichever side
  dials the connection. gRPC client and server are wire roles and imply
  nothing about who is in charge.
- **Money is an integer count of paise.** Rupees appear only at the API
  boundary, where a human writes a fixture.
- **Test-mode Razorpay keys are enforced at startup.** The service refuses
  to run against a live key, because it generates payment demands
  autonomously from a conversation.

---

## 6. Running it

Start order matters, each service depends on the previous being up:
Inference-Python (`:50051`), Orchestrator-Go gateway (`:50052`), AetherRTC
(`:8080`), then the browser. The recovery orchestrator (`:8090`) is
independent and can start any time before a call is placed.

`var-thon/services/inference-py` uses `uv` with Python 3.11 pinned. Runtime
dependencies not in the repository: `ollama pull qwen2.5:3b` and the Piper
voice model at `models/en_US-lessac-medium.onnx`.

Full instructions are in `docs/setup.md`.
