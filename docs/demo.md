# Vasuli — Demo Runbook

The complete cycle, top to bottom: voice call → outcome classification →
payment link → webhook → recovered, with the audit trail proving each step.

Every command here has been run against the real system. Where a step has a
non-obvious failure mode, it is called out inline rather than left to be
rediscovered.

---

## Before you start

**Warm the LLM.** A cold Ollama load costs 5–10s on the first response,
which reads as a broken system to anyone watching:

```powershell
curl.exe -s -X POST http://localhost:11434/api/generate -d '{\"model\":\"qwen2.5:3b\",\"prompt\":\"hi\",\"stream\":false}' > $null
```

**Start from a clean database** unless you deliberately want prior state:

```powershell
cd E:\APPLICATIONS\CODING\project\vasuli\recovery-orchestrator
Remove-Item vasuli.db -ErrorAction SilentlyContinue
```

---

## Terminal 1 — Recovery Orchestrator

```powershell
cd E:\APPLICATIONS\CODING\project\vasuli\recovery-orchestrator
go run ./cmd -port :8090 -db ./vasuli.db -razorpay-webhook-secret whsec_vasuli_demo
```

Wait for: `Recovery Orchestrator listening on :8090 (db: ./vasuli.db, razorpay: stub (no credentials supplied))`

`whsec_vasuli_demo` is a secret you choose. It is shared between this
server and the `curl` commands below — Razorpay never sees it and no
webhook registration is required. Without this flag, webhook verification
fails closed and every webhook is rejected.

To run against the real Razorpay test API instead, add
`-razorpay-key-id rzp_test_... -razorpay-key-secret ...`. Startup refuses
any key without an `rzp_test_` prefix.

## Terminal 2 — Inference engine

```powershell
cd E:\APPLICATIONS\CODING\project\vasuli\var-thon\services\inference-py
uv run python main.py
```

Wait for: `Inference Engine running on port :50051`

## Terminal 3 — Orchestrator-Go gateway

```powershell
cd E:\APPLICATIONS\CODING\project\vasuli\var-thon\services\orchestrator-go
go run ./cmd/gateway-server -profile recovery_agent -port :50052 -inference localhost:50051 -recovery http://localhost:8090
```

Wait for: `Gateway server listening on :50052 (profile: Priya, ... recovery: http://localhost:8090)`

Confirm it says **`profile: Priya`**. If it says `Sarah the Receptionist`,
the wrong profile is loaded and the fallback persona will be a dental
receptionist.

## Terminal 4 — AetherRTC

```powershell
cd E:\APPLICATIONS\CODING\project\AetherRTC
go run ./cmd/gateway/main.go
```

Wait for: `Listening on ws://localhost:8080/ws`

---

## Step 1 — Load the campaign

```powershell
cd E:\APPLICATIONS\CODING\project\vasuli\recovery-orchestrator
curl.exe -s -X POST http://localhost:8090/api/v1/campaigns -H "Content-Type: application/json" -d "@testdata/m3_smoke_test.json"
```

```json
{"campaign_id":"camp_...","name":"M3 Smoke Test","total":1,"status":"active",...}
```

Note the `campaign_id` — Step 5 needs it.

**The fixture holds one account.** Any earlier assign, including a probe
with `gateway-test-client`, consumes it and leaves the queue empty — at
which point the agent falls back to the static profile and never mentions
the customer. If a call comes through generically, re-run this command.

## Step 2 — The live call

Open `E:\APPLICATIONS\CODING\project\AetherRTC\index.html`, click
**Start Call**, allow the microphone, wait for
`Received remote audio track`.

**The single most important rule: wait for Priya to finish speaking before
you talk.** The microphone is gated at the edge for the entire duration of
her audio playback, so anything said over her is discarded before it
reaches the pipeline — indistinguishable from the system being broken.

Do not use the logs for timing. Terminal 2 prints
`Utterance response complete. 6.30s of audio queued; caller audio is gated
at the edge until playback ends` — synthesis finishes *seconds before*
playback does. That number is how long you must wait. **Go by your ears,
then leave a beat.**

A conversation that reaches the AGREED path:

1. "Hello." — wait
2. "Yes, you are speaking with Rahul Sharma." — wait
3. "Yes, it is a convenient time." — wait, she states ₹4,200
4. **"Please give me the payment link."** — wait for her confirmation
5. Pause ~2 seconds of silence so VAD closes the utterance
6. Click **End Call**

Step 5 matters: if you hang up inside VAD's 800ms silence threshold, the
final utterance never reaches the transcript and the call may classify
`UNCLEAR` instead of `AGREED`.

**What to watch.** Terminal 2, after you hang up:

```
[session_...] Inbound stream closed. Signaling shutdown.
[session_...] Outcome classified as AGREED in 0.858s.
```

Terminal 3:

```
[Session session_...] Call outcome classified: AGREED
[Gateway] session session_...: reported outcome 'AGREED' to recovery orchestrator.
```

## Step 3 — Audit trail

```powershell
sqlite3 -header -column E:\APPLICATIONS\CODING\project\vasuli\recovery-orchestrator\vasuli.db "SELECT event_type, event_data FROM audit_log ORDER BY id;"
```

```
session_created     {"customer_name":"Rahul Sharma"}
session_assigned    {"call_session_id":"session_..."}
call_ended          {"outcome":"AGREED"}
outcome_classified  {"outcome":"AGREED"}
payment_link_sent   {"amount_paise":420000,"razorpay_link_id":"plink_...",...}
```

Every event timestamped, from queueing through the spoken conversation to
the payment link. This is what the judging bar asks for.

## Step 4 — Session state

```powershell
sqlite3 -header -column E:\APPLICATIONS\CODING\project\vasuli\recovery-orchestrator\vasuli.db "SELECT customer_name, status, razorpay_link_id FROM recovery_sessions;"
```

Status is **`link_sent`**, not `recovered`. That distinction is deliberate:
the customer agreed and a link exists, but no payment is confirmed.
Claiming `recovered` here would overstate what happened. Step 5 is what
makes it `recovered`.

## Step 5 — Confirm payment via webhook

**Git Bash** (needs `openssl`):

```bash
cd /e/APPLICATIONS/CODING/project/vasuli/recovery-orchestrator
SECRET=whsec_vasuli_demo
LINK=$(sqlite3 vasuli.db "SELECT razorpay_link_id FROM recovery_sessions WHERE status='link_sent' LIMIT 1;")
BODY="{\"event\":\"payment_link.paid\",\"payload\":{\"payment_link\":{\"entity\":{\"id\":\"$LINK\",\"status\":\"paid\"}},\"payment\":{\"entity\":{\"id\":\"pay_demo_capture_1\",\"status\":\"captured\"}}}}"
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $2}')

curl -s -X POST http://localhost:8090/webhooks/razorpay \
  -H "Content-Type: application/json" \
  -H "X-Razorpay-Signature: $SIG" \
  -d "$BODY"
```

Terminal 1:
`[Recovery] payment_link.paid confirmed recovery for link plink_... (payment pay_demo_capture_1)`

```powershell
sqlite3 -header -column E:\APPLICATIONS\CODING\project\vasuli\recovery-orchestrator\vasuli.db "SELECT customer_name, status, recovered_at FROM recovery_sessions;"
```

Now `recovered`.

**Worth saying out loud to judges:** the payment id in that webhook,
`pay_demo_capture_1`, appears nowhere in the database. A customer paying a
generated link creates a *new* payment unrelated to the failed one that
started recovery, so the session is resolved by **payment link id**, not
payment id. Matching on the payment id — the obvious-looking choice — would
silently resolve nothing.

## Step 6 — Metrics

```powershell
curl.exe -s http://localhost:8090/api/v1/campaigns/<campaign_id>/metrics
```

```json
{"total_accounts":1,"contacted":1,
 "breakdown":{"recovered":1,"recovered_amount_paise":420000,...},
 "pending":0,"payment_links_sent":1,"razorpay_captured_confirmed":1}
```

---

## Optional: inbound `payment.failed`

Demonstrates that a real Razorpay event can seed recovery, not just a
manually loaded batch. Requires an **active campaign** to attach to.

```bash
SECRET=whsec_vasuli_demo
BODY='{"event":"payment.failed","payload":{"payment":{"entity":{"id":"pay_inbound_demo","amount":189000,"currency":"INR","description":"Airtel Postpaid Bill","notes":{"customer_name":"Preethi Nair"}}}}}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $2}')
curl -s -X POST http://localhost:8090/webhooks/razorpay -H "Content-Type: application/json" -H "X-Razorpay-Signature: $SIG" -d "$BODY"
```

Preethi Nair is now queued and will be assigned to the next call.

---

## Timing targets

| Moment | Target |
| ------ | ------ |
| Start Call → Priya's first word | < 3s (warm model) |
| Utterance ends → Priya starts responding | < 2s |
| Hang up → outcome in `audit_log` | < 5s |

Measured on RTX 3050 Mobile 4GB / Ryzen 5 6600H: STT ~0.67s, LLM TTFT
~0.51s, classification ~0.86s, full teardown ~1s.

---

## When something breaks

**Priya uses a generic greeting, no customer name.** Queue was empty —
Terminal 3 says `recovery queue empty, using static profile 'Priya'`.
Re-run Step 1.

**Priya is a dental receptionist.** Gateway started with
`-profile receptionist`. Restart with `-profile recovery_agent`.

**Nothing is detected after the first exchange.** You spoke while Priya's
audio was still playing. Wait for her to finish, then leave a beat. See
Step 2.

**Outcome is UNCLEAR after a clear agreement.** Known limitation of the 3B
classifier: it over-weights the final turn, so a closing "no thanks, bye"
can read as refusal. The system degrades safely — no payment link is
created on an unclear call. Manual override:

```bash
curl -s -X POST http://localhost:8090/api/v1/calls/<call_session_id>/end -H "Content-Type: application/json" -d '{"outcome":"AGREED"}'
```

**Webhook returns 400.** The secret in your shell does not match
`-razorpay-webhook-secret`, or the body was modified after signing. Sign
the exact bytes you send.

**Payment link creation fails against live keys.** The error carries
Razorpay's own description — check Terminal 1. Confirm the key starts with
`rzp_test_`.
