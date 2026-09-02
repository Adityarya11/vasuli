# Vasuli, Setup Guide

## Repository Structure

```
vasuli/
├── README.md
├── var-thon/                  Voice Agent Runtime (adapted from VAR)
│   ├── services/
│   │   ├── orchestrator-go/   Go service, port :50052
│   │   └── inference-py/      Python service, port :50051
│   └── proto/
└── recovery-orchestrator/     New Go service, port :8090
```

AetherRTC runs from its own repository. It is not included in this monorepo.

---

## Hardware Requirements

The inference pipeline runs entirely on local hardware. Minimum tested configuration:

| Component  | Minimum                 | Tested on           |
| ---------- | ----------------------- | ------------------- |
| GPU (VRAM) | 4GB                     | RTX 3050 Mobile 4GB |
| CPU        | 6 cores                 | Ryzen 5 6600H       |
| RAM        | 16GB                    | 16GB DDR5           |
| OS         | Linux (WSL2 acceptable) | Ubuntu 24 via WSL2  |

GPU is used exclusively for the LLM. STT (Faster-Whisper) and TTS (Piper) run on CPU.

---

## Dependencies

### System

```bash
# Go 1.21+
go version

# Python 3.11 + uv
python3 --version
uv --version

# Ollama
ollama --version
```

### Models

```bash
# Pull the LLM
ollama pull qwen2.5:3b

# Download Piper TTS voice model
mkdir -p var-thon/services/inference-py/models
cd var-thon/services/inference-py/models

# en_US-lessac-medium (female, neutral American English, 22kHz)
wget https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/lessac/medium/en_US-lessac-medium.onnx
wget https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/lessac/medium/en_US-lessac-medium.onnx.json

# Silero VAD model
wget https://github.com/snakers4/silero-vad/raw/master/src/silero_vad/data/silero_vad.onnx
```

### Python packages

```bash
cd var-thon/services/inference-py
uv sync
```

### Go modules

```bash
# var-thon orchestrator
cd var-thon/services/orchestrator-go
go mod download

# Recovery Orchestrator
cd recovery-orchestrator
go mod download
```

---

## AetherRTC

Clone and build separately:

```bash
git clone https://github.com/Adityarya11/aetherRTC
cd aetherRTC
go mod download
```

AetherRTC requires no configuration changes. It points to `localhost:50052` (var-thon Orchestrator-Go) by default.

---

## Razorpay Test-Mode Setup

1. Create an account at [razorpay.com](https://razorpay.com) if you don't have one
2. Switch to **Test Mode** in the Dashboard
3. Settings → API Keys → Generate Test Key Pair → copy Key ID and Key Secret

That is the entire setup. Test-mode keys require no website and no KYC,
those apply only to live mode, which Vasuli refuses to run against.

**Do not add a webhook in the Dashboard.** That form requires a public
HTTPS URL, which would mean hosting or a tunnel. Vasuli signs its own
webhook requests locally instead: the secret is a string you choose, shared
between the server and the `curl` that sends the event. The verification
path exercised is identical either way, without the external dependency.
See `docs/demo.md` Step 5.

---

## Environment Variables

Create `recovery-orchestrator/.env` (gitignored, never commit it):

```env
RAZORPAY_KEY_ID=rzp_test_XXXXXXXXXXXX
RAZORPAY_KEY_SECRET=YYYYYYYYYYYYYYYYYYYY
RAZORPAY_WEBHOOK_SECRET=whsec_vasuli_demo
```

`RAZORPAY_WEBHOOK_SECRET` is invented by you, not issued by Razorpay. Only
the first two come from the Dashboard.

The file is read at startup relative to the working directory, so start the
service from `recovery-orchestrator/`. Precedence is **flag > shell
environment > `.env`**, so a flag can still override for a single run:

```bash
go run ./cmd -razorpay-key-id rzp_test_OTHER -razorpay-key-secret OTHER
```

With no credentials at all the service runs against a stub client that
fabricates payment links locally, every endpoint including the webhook
consumer still works.

---

## Start Order

Services must start in this order. Each depends on the one before it being ready.

```bash
# 1. Inference-Python (waits for Ollama and model files)
cd var-thon/services/inference-py
uv run python main.py

# 2. var-thon Orchestrator-Go
cd var-thon/services/orchestrator-go
go run ./cmd/gateway-server \
  -profile recovery_agent \
  -port :50052 \
  -inference localhost:50051 \
  -recovery http://localhost:8090

# 3. Recovery Orchestrator
cd recovery-orchestrator
go run ./cmd -port :8090 -db ./vasuli.db   # credentials come from .env

# 4. AetherRTC
cd aetherRTC
go run ./cmd/gateway/main.go

# 5. Browser
# Open aetherRTC/index.html
```

---

## Verifying Each Service

```bash
# Inference-Python: should log "running on port :50051"
# var-thon: should log "Gateway server listening on :50052"

# Recovery Orchestrator health check
curl http://localhost:8090/health
# → {"status":"ok","db":"connected"}

# AetherRTC: should log "Listening on ws://localhost:8080/ws"
```

---

## First Run Checklist

- [ ] `ollama list` shows `qwen2.5:3b`
- [ ] Silero VAD ONNX at `var-thon/services/inference-py/models/silero_vad.onnx`
- [ ] Piper model at `var-thon/services/inference-py/models/en_US-lessac-medium.onnx`
- [ ] Inference-Python starts without errors
- [ ] var-thon connects to Inference-Python (no "connection refused" in logs)
- [ ] Recovery Orchestrator starts and creates `vasuli.db`
- [ ] AetherRTC starts without errors
- [ ] Browser tab connects and Priya responds within 5 seconds

The first response on a cold system (Piper ONNX initialization + Ollama model load) takes 5–10 seconds. All subsequent calls in the same process are at normal latency.
