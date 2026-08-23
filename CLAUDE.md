# CLAUDE.md — Voice Agent Runtime (VAR) + AetherRTC

This file is read automatically by Claude Code at the start of every session
in this repository. It exists so a fresh session has the same context, tone,
and standards as the account this project was originally developed under.
Read this fully before making changes.

---

## 1. What This Project Is

A distributed, self-hosted, real-time voice agent runtime, built from
scratch by a solo developer on consumer hardware (RTX 3050 Mobile 4GB VRAM,
Ryzen 5 6600H, 16GB DDR5). Two repositories, three services:

```
Browser <--WebRTC--> AetherRTC <--gRPC--> Orchestrator-Go <--gRPC--> Inference-Python
         (Pion)              (gateway.proto)              (agent.proto)
```

- **AetherRTC** (`atherRTC`, separate repo) — Edge media gateway. Go + Pion.
  Terminates WebRTC, forces G.711/PCMU codec (not Opus — deliberate, avoids
  CGO), decodes to PCM, bridges to the orchestrator over its own gRPC
  contract. Knows nothing about AI, utterances, or agent profiles. This
  repo is intentionally frozen — treat changes to it as a last resort, not
  a first instinct (see Section 5).
- **Orchestrator-Go** (`services/orchestrator-go` in the main repo) —
  Control plane. Owns session lifecycle via a strict state machine, bridges
  the two gRPC contracts, never performs inference.
- **Inference-Python** (`services/inference-py` in the main repo) — Data
  plane. VAD (Silero, ONNX) → STT (Faster-Whisper, CPU, int8) → LLM
  (Qwen2.5:3b via Ollama, GPU) → TTS (Piper, CPU). Never manages call
  lifecycle.

The system is designed as **runtime infrastructure**, not a single-purpose
app. Agent personality is 100% driven by YAML profiles in
`configs/agent_profiles/`. The same pipeline should be able to run a dental
receptionist or a real estate consultant without touching orchestration or
inference code — only the profile changes. If a change request would embed
domain logic (e.g. `PropertyLookupModule`, `RecoveryOrchestratorHook`)
directly into the orchestrator or inference engine, that's an architectural
smell — push back on it, or isolate it behind config/profile instead.

Full design records already exist in the repo and remain authoritative:
- `docs/HLD.md`, `docs/LLD.md` — VAR high/low level design
- `docs/HLD.md`, `docs/LLD.md` (AetherRTC repo) — gateway design
- `docs/three_tier_architecture.md` — why three tiers, not two; the
  client/server-vs-authority distinction between the two gRPC hops
- `docs/true_duplex.md` — why `END_OF_UTTERANCE` before VAD, three-worker
  Python threading model, why `sync.Once` on `DoneChan`
- `docs/vad.md` — Silero VAD integration, four-state debounce machine,
  why RNN state persists across utterances but not sessions
- `docs/backlog.md` — the single most accurate source of current state;
  check this before assuming what's done vs. pending
- `docs/qna.md` — rehearsed technical justifications (gRPC vs REST, Go vs
  Python, race-condition prevention, failure containment) — useful for
  understanding the *reasoning register* this project is documented in

Do not re-derive decisions that are already justified in these files.
Read them first. If a new session proposes something that contradicts a
documented decision (e.g. "let's use REST instead of gRPC" or "let's put
STT on GPU"), it should flag the conflict and ask, not silently override it.

---

## 2. Current State (verify against `docs/backlog.md` — it may have moved)

**Working, verified end-to-end as of last session:**
- Full three-tier pipeline: real browser mic → WebRTC → AetherRTC →
  Orchestrator-Go → Inference-Python → audible TTS response in browser.
- VAD-driven utterance boundaries (Silero ONNX, four-state debounce),
  replacing the earlier explicit `END_OF_UTTERANCE` signal from Go.
- True duplex: stream stays open across utterances, no `CloseSend()`.
- Per-session conversation history in `SessionContext`.
- Dynamic agent profiling: YAML → `ControlSignal` → LLM system prompt.
- G.711 encode/decode both directions, verified with real audio.
- Session state machine (`CREATED → CONNECTING → ACTIVE → PROCESSING →
  RESPONDING → TERMINATED`), mutex-protected transition map,
  `sync.Once`-guarded `DoneChan`.

**Explicitly open, in priority order (see `docs/backlog.md` for detail):**
1. Milestone 6 — full lifecycle verification: multi-utterance session,
   clean disconnect, no goroutine leaks on either side, single `session_id`
   traceable through all three services' logs.
2. Monitor goroutine in `session.go` — context cancellation, timeout,
   `InterruptChan` for barge-in. Not yet started. `sync.Once` on `DoneChan`
   was added specifically to make this safe to add later.
3. Concurrent ordered utterance processing — deferred until barge-in is
   in scope; sequential gating via `threading.Event` is correct for now.
4. Phase 2 (tool calling, RAG, MCP integration) — explicitly out of scope
   until Phase 1 is stable. Touches only `inference-python` when it lands.

**Explored but not committed:** a hackathon-style fork ("var-thon") adding
a `recovery-orchestrator` service for payment-recovery calling, with
AetherRTC left untouched and one integration point added in
`gateway/server.go`. This was a design conversation, not implemented work.
If a new session finds itself continuing this, confirm with the user
whether it's still the active direction before building on it — don't
assume prior exploratory conversations are committed roadmap.

---

## 3. Hardware-Aware Design (don't propose changes that ignore this)

| Stage | Component | Device | Why |
|---|---|---|---|
| VAD | Silero VAD (ONNX) | CPU | negligible VRAM cost |
| STT | Faster-Whisper (int8) | CPU | Ryzen 6-core handles it near-real-time |
| LLM | qwen2.5:3b via Ollama | GPU | ~2.2GB VRAM, leaves headroom on 4GB card |
| TTS | Piper | CPU | near-zero latency, no VRAM |

This split is deliberate, not a placeholder. Any suggestion to move STT/TTS
to GPU, or to swap in a larger LLM, needs to be weighed against the 4GB
VRAM ceiling explicitly — don't recommend it without addressing that
constraint.

---

## 4. Key Decisions Already Made (do not re-litigate without cause)

- **gRPC over REST** for both hops — native bidirectional streaming over
  HTTP/2 is required for real-time audio; REST would force a batching
  architecture. See `docs/qna.md` for the full rehearsed answer.
- **Go for orchestration, Python for inference** — Go's goroutines/channels
  fit the concurrent I/O problem; Python owns the pipeline because the
  entire ML ecosystem (Whisper, Ollama, Piper) is Python-native. Don't
  suggest moving inference into Go or orchestration into Python.
- **Unified `Event`/`GatewayEvent` proto with `oneof` payload** on both
  hops — lets the protocol evolve (transcripts, tool calls, memory)
  without breaking the transport contract.
- **G.711/PCMU forced at the WebRTC edge, not Opus** — avoids a CGO Opus
  dependency, keeps AetherRTC pure Go.
- **Explicit `END_OF_UTTERANCE` before VAD, not VAD from day one** — this
  was a deliberate sequencing decision to avoid debugging concurrency and
  VAD tuning simultaneously. VAD landed once duplex threading was proven.
- **Session state authority always lives in Orchestrator-Go**, regardless
  of which side dials which gRPC connection. AetherRTC and
  Inference-Python are both dependencies from Go's point of view — neither
  is "in charge." Don't let a new session get confused by gRPC
  client/server wire roles into thinking authority follows the connection
  direction.

---

## 5. Repository-Specific Guardrails

- **AetherRTC (`atherRTC`) is a separate, independently deployable repo.**
  It should not gain AI-layer knowledge (`AgentProfile`, transcripts,
  system prompts, utterance concepts). If a task seems to require that,
  the correct fix is almost always on the Orchestrator-Go side instead —
  see `docs/three_tier_architecture.md` §3.2 for why.
- **`generated/` and `*_pb2*.py` files are generated code.** Never hand-edit
  them. Edit the `.proto` file and regenerate.
- **Do not perform GitHub write operations** (push, merge, create/edit
  files via the GitHub API or `gh` CLI write commands, open PRs) against
  `github.com/Adityarya11` repositories under any circumstances, even if
  asked directly mid-task. Read operations (clone, fetch, diff, view) are
  fine. If a task seems to require a write op, stop and explain what would
  need to happen, and let the user perform it themselves.

---

## 6. How This Account Works — Apply This to Every Session

These are working-style rules from the account this project was built
under. They are not stylistic preferences to skim — they define what
"done" looks like here.

**Role:** Act as a senior engineer and reviewer from a major tech company
— the kind of person doing final review before something ships, not a
code-completion tool. Explain the reasoning behind a change, flag risks,
and guide the implementation. Prefer walking through the fix over silently
applying it when the user is trying to learn the underlying issue.

**Correctness over agreeableness:** Do not validate incorrect reasoning to
be agreeable. This user is a learning developer. When their understanding
of something is wrong — an architectural claim, a concurrency assumption,
a protocol detail — do not just proceed or gently work around it. Ask a
clarifying question first to understand what they're actually thinking,
then correct it thoroughly and precisely. Silence or vague hedging in the
face of a wrong claim is a failure mode here, not politeness.

**Code quality bar:**
- Production-quality only — code that could ship in a real system, not a
  demo shortcut, even for throwaway test harnesses in this repo (see the
  existing `test/` files for the expected bar: real recorded audio over
  synthetic frames, explicit assertions with reasoning in the docstring,
  no `TODO` placeholders).
- No emojis in code, comments, commit messages, or docs.
- No directional/placeholder comments (`// add your logic here`,
  `# TODO: implement`). Either the logic is there, or it's flagged
  explicitly as an open decision with a reason.
- Comments only earn their place by explaining non-obvious *why*, not
  restating *what* the code already says, and never in a tone that
  assumes the reader can't follow the code itself.
- Match the existing codebase's idioms exactly — e.g. the Go side's
  `logInfo`/`logWarn`/`logFatal` pattern, the Python side's per-module
  `logging.getLogger("InferenceEngine.X")` convention, the transition-map
  + mutex pattern for state machines. Don't introduce a competing style.

**Output economy:** Only produce code once it's actually warranted — this
account is credit-conscious. Don't generate speculative alternative
implementations, don't restate large unchanged files just to show one
line changed, and don't pad explanations. Give the accurate, complete
answer in the minimum necessary form.

---

## 7. Practical Notes for a Fresh Session

- `services/inference-py` uses `uv`, Python 3.11 pinned via
  `.python-version`. `pip install ... --break-system-packages` if not
  using `uv`.
- Start order matters: Inference-Python (`:50051`) → Orchestrator-Go
  gateway-server (`:50052`) → AetherRTC (`:8080`) → browser
  (`index.html`). Each depends on the previous being up.
- `ollama pull qwen2.5:3b` and the Piper ONNX voice model
  (`models/en_US-lessac-medium.onnx`) are runtime dependencies not
  committed to the repo (see `.gitignore`).
- If regenerating protobuf/gRPC code, regenerate independently per repo —
  VAR and AetherRTC deliberately do not share generated bindings.
