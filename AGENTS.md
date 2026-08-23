# AGENTS.md — Voice Agent Runtime (VAR) + AetherRTC

This is a general-purpose agent instruction file for any tool that reads
`AGENTS.md` instead of, or in addition to, `CLAUDE.md`. The full,
authoritative project context lives in `CLAUDE.md` at this repository
root — read that file in full before doing anything else. This file
exists only so tools that don't recognize `CLAUDE.md` still pick up the
essentials.

## Project in one paragraph

A self-hosted, real-time voice agent runtime split across three services:
AetherRTC (Go, Pion, WebRTC edge gateway — separate repo, treat as frozen
unless there's no alternative), Orchestrator-Go (session lifecycle,
control plane), Inference-Python (VAD → STT → LLM → TTS pipeline, data
plane). Agent behavior is driven entirely by YAML profiles, never
hardcoded. See `docs/HLD.md`, `docs/LLD.md`, `docs/three_tier_architecture.md`,
`docs/backlog.md` for full design rationale and current status before
proposing architectural changes.

## Non-negotiable rules

1. Never perform GitHub write operations (push, PR, merge, API/CLI writes)
   against `github.com/Adityarya11` repositories. Read-only is fine.
2. Never hand-edit generated files (`generated/`, `*_pb2*.py`). Edit the
   `.proto` and regenerate.
3. Do not add AI-layer concepts (agent profiles, transcripts, utterances)
   into the AetherRTC repo. That knowledge belongs in Orchestrator-Go.
4. Production-quality code only. No emojis. No placeholder/directional
   comments. Comments explain non-obvious *why*, not *what*.
5. Act as a senior reviewer, not an autocomplete tool: explain reasoning,
   flag risks, and when the user's stated understanding is wrong, ask a
   clarifying question first, then correct it precisely — don't quietly
   agree or work around it.
6. Be economical with output — no speculative alternatives, no restating
   unchanged code, no padding.

Full detail on all of the above, plus current milestone status, hardware
constraints, and prior design decisions, is in `CLAUDE.md`.
