# Vasuli, Demo Runbook

The complete cycle, top to bottom: voice call → outcome classification →
payment link → webhook → recovered, with the audit trail proving each step.

Every command here has been run against the real system. Where a step has a
non-obvious failure mode, it is called out inline rather than left to be
rediscovered.

---

## Credentials

`recovery-orchestrator/.env` holds the Razorpay test-mode credentials. It is
gitignored and must never be committed.

```env
RAZORPAY_KEY_ID=rzp_test_XXXXXXXXXXXXXX
RAZORPAY_KEY_SECRET=XXXXXXXXXXXXXXXXXXXXXXXX
RAZORPAY_WEBHOOK_SECRET=whsec_vasuli_demo
```

Precedence is **CLI flag > shell environment > `.env`**, so the flags still
work if you need to override for one run. The file is read relative to the
working directory, so Terminal 1 must start from
`recovery-orchestrator/`.

`RAZORPAY_WEBHOOK_SECRET` is a string **you invent**. It is shared between
this server and the `curl` in Step 5, and Razorpay never sees it.

> **Do not create a webhook in the Razorpay dashboard.** That form demands a
> public HTTPS URL, which would mean hosting or a tunnel, an external
> dependency that can fail mid-demo. Vasuli signs its own webhook requests
> locally, and the verification code exercised is byte-identical to what a
> real delivery would hit. Only the API keys come from Razorpay.

---

## Before you start

**Warm the LLM.** A cold load costs 5–10s on the first response, which
reads as a broken system:

```powershell
curl.exe -s -X POST http://localhost:11434/api/generate -d '{\"model\":\"qwen2.5:3b\",\"prompt\":\"hi\",\"stream\":false}' > $null
```

**Start from a clean database** unless you deliberately want prior state:

```powershell
cd E:\APPLICATIONS\CODING\project\vasuli\recovery-orchestrator
Remove-Item vasuli.db -ErrorAction SilentlyContinue
```

---

## Terminal 1, Recovery Orchestrator

```powershell
cd E:\APPLICATIONS\CODING\project\vasuli\recovery-orchestrator
go run ./cmd -port :8090 -db ./vasuli.db
```

Startup must read:

```
Recovery Orchestrator listening on :8090 (db: ./vasuli.db, razorpay: live (test mode))
```

| Startup says                     | Meaning                                                                                |
| -------------------------------- | -------------------------------------------------------------------------------------- |
| `live (test mode)`               | Real Razorpay API. Payment links are genuine and appear in your dashboard              |
| `stub (no credentials supplied)` | `.env` not found, you are in the wrong directory. Links will be fake `plink_stub_...` |
| refuses to start                 | The key is not `rzp_test_`. Vasuli will not run against live credentials               |

A `No -razorpay-webhook-secret set` warning means Step 5 will reject every
webhook.

## Terminal 2, Inference engine

```powershell
cd E:\APPLICATIONS\CODING\project\vasuli\var-thon\services\inference-py
.venv\Scripts\Activate.ps1
uv run python main.py
```

Wait for: `Inference Engine running on port :50051`

## Terminal 3, Orchestrator-Go gateway

```powershell
cd E:\APPLICATIONS\CODING\project\vasuli\var-thon\services\orchestrator-go
go run ./cmd/gateway-server -profile recovery_agent -port :50052 -inference localhost:50051 -recovery http://localhost:8090
```

Confirm it says **`profile: Priya`**. If it says `Sarah the Receptionist`,
the wrong profile is loaded and the fallback persona is a dental
receptionist.

## Terminal 4, AetherRTC

```powershell
cd E:\APPLICATIONS\CODING\project\AetherRTC
./index.html
go run ./cmd/gateway/main.go
```

Wait for: `Listening on ws://localhost:8080/ws`

---

## Step 0, Preflight

Two seconds, and it catches the single most common failure: a service that
exited without anyone noticing, leaving stale scrollback in a terminal that
looks alive.

```powershell
foreach ($p in 8090,50051,50052,8080,11434) {
  $up = Get-NetTCPConnection -LocalPort $p -State Listen -ErrorAction SilentlyContinue
  "{0,-6} {1}" -f $p, $(if ($up) {"UP"} else {"** DOWN **"})
}
```

All five must read `UP`. If the browser connects but **nothing at all
happens**, this is almost always why, that symptom means a service is
down, and is different from "one exchange then silence", which means you
spoke too early (see Step 2).

## Step 1, Load the campaign

```powershell
cd E:\APPLICATIONS\CODING\project\vasuli\recovery-orchestrator
curl.exe -s -X POST http://localhost:8090/api/v1/campaigns -H "Content-Type: application/json" -d "@testdata/sample_campaign.json"
```

Use `m3_smoke_test.json` instead for a single-account run.

Amounts in the fixture are **whole rupees** (`"outstanding_rupees": 4200`).
They are stored and sent to Razorpay as paise, because money belongs in an
integer count of the smallest unit, the conversion happens once, at the
API boundary.

```json
{"campaign_id":"camp_...","name":"M3 Smoke Test","total":1,"status":"active",...}
```

Note the `campaign_id`, Step 6 needs it.

`curl: option -d: error encountered when reading a file` means you are not
in `recovery-orchestrator/`; the `@` path is relative.

**The fixture holds one account.** Any earlier assign consumes it and
leaves the queue empty, at which point the agent falls back to the static
profile and never mentions the customer. If a call comes through
generically, re-run this command.

## Step 1b, Show who the system decided to call

```powershell
curl.exe -s http://localhost:8090/api/v1/campaigns/camp_XXXXXXXX/queue
```

```json
{
  "due_now": [
    {"customer_name":"Rahul Sharma","outstanding_rupees":4200,"attempt":"0 of 3","status":"pending"}
  ],
  "on_hold": [],
  "closed": []
}
```

This is the outbound decision made visible: **the system selected this
customer**, before any call exists. Worth showing before the call rather
than after, because it is what makes the workflow outbound-shaped even
though the transport is a browser session the customer joins.

## Step 2, The live call

Open `E:\APPLICATIONS\CODING\project\AetherRTC\index.html`, click
**Start Call**, allow the microphone, wait for
`Received remote audio track`.

**The single most important rule: wait for Priya to finish speaking before
you talk.** The microphone is gated at the edge for the entire duration of
her audio playback, so anything said over her is discarded before it
reaches the pipeline. Indistinguishable from the system being broken.

Do not use the logs for timing. Terminal 2 prints
`Utterance response complete. 9.15s of audio queued; caller audio is gated
at the edge until playback ends`, synthesis finishes _seconds before_
playback does. That number is how long you must wait. **Go by your ears,
then leave a beat.**

Replies run long, 15–30s of audio is normal, which is a known limitation
of the 3B model (see `docs/build-log.md`). Budget roughly three exchanges
per minute and keep your own answers short.

A conversation that reaches the AGREED path:

1. "Hello.", wait
2. "Yes, you are speaking with Rahul Sharma.", wait
3. **"Please give me the payment link."**, wait for her confirmation
4. Optionally "Thank you.", wait
5. Pause ~2 seconds of silence so VAD closes the utterance
6. Click **End Call**

Two things matter here, both measured:

**Never end with a phrase containing "no".** "No thanks, bye" classifies
the whole call as `REFUSED`, three times out of three, even after a clear
agreement, because the classifier reads that "no" as refusing the payment.
"Thank you." classifies `AGREED` three times out of three. Close politely
or simply stop talking.

**Leave the silence before hanging up.** Cutting off inside VAD's 800ms
threshold loses the final utterance, and the call may classify `UNCLEAR`.

**What to watch.** Terminal 2, after you hang up:

```
[session_...] Inbound stream closed. Signaling shutdown.
[session_...] Outcome classified as AGREED in 0.745s.
```

Terminal 3:

```
[Session session_...] Call outcome classified: AGREED
[Gateway] session session_...: reported outcome 'AGREED' to recovery orchestrator.
```

## Step 3, Audit trail

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
the payment link.

## Step 4, Session state

```powershell
sqlite3 -header -column E:\APPLICATIONS\CODING\project\vasuli\recovery-orchestrator\vasuli.db "SELECT customer_name, status, razorpay_link_id FROM recovery_sessions;"
```

Status is **`link_sent`**, not `recovered`. That distinction is deliberate:
the customer agreed and a link exists, but no payment is confirmed.
Claiming `recovered` here would overstate what happened. Step 5 is what
makes it `recovered`.

If `razorpay_link_id` starts `plink_stub_`, Terminal 1 was in stub mode. A
genuine link is `plink_` with no `_stub_`, and **opens as a real payment
page**. Worth showing judges alongside the Razorpay dashboard.

## Step 5, Confirm payment via webhook

**Git Bash** (needs `openssl`). Absolute DB path and an empty-link guard,
because both have bitten this runbook before:

```bash
DB=/e/APPLICATIONS/CODING/project/vasuli/recovery-orchestrator/vasuli.db
SECRET=whsec_vasuli_demo
LINK=$(sqlite3 "$DB" "SELECT razorpay_link_id FROM recovery_sessions WHERE status='link_sent' LIMIT 1;")

if [ -z "$LINK" ]; then echo "ERROR: no link found - wrong db path, or no AGREED call yet"; else
  BODY="{\"event\":\"payment_link.paid\",\"payload\":{\"payment_link\":{\"entity\":{\"id\":\"$LINK\",\"status\":\"paid\"}},\"payment\":{\"entity\":{\"id\":\"pay_demo_capture_1\",\"status\":\"captured\"}}}}"
  SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $2}')
  echo "sending for link: $LINK"
  curl -s -X POST http://localhost:8090/webhooks/razorpay \
    -H "Content-Type: application/json" \
    -H "X-Razorpay-Signature: $SIG" \
    -d "$BODY"
  echo
  sqlite3 -header -column "$DB" "SELECT customer_name, status, recovered_at FROM recovery_sessions WHERE razorpay_link_id='$LINK';"
fi
```

**Type these commands or paste through a plain text editor first.** Pasting
from formatted text can carry a Unicode en-dash that looks identical to a
hyphen and produces `command not found`. When that happened, `cd` silently
failed, `sqlite3` opened an empty database in the home directory, `LINK`
came back empty, and the server correctly reported
`payment_link.paid for unknown link  - acknowledged, no action`, note the
blank where the id should be. That blank is the tell.

Terminal 1 on success:
`[Recovery] payment_link.paid confirmed recovery for link plink_... (payment pay_demo_capture_1)`

Status is now `recovered` with a timestamp.

**Worth saying out loud to judges:** the payment id in that webhook,
`pay_demo_capture_1`, appears nowhere in the database. A customer paying a
generated link creates a _new_ payment unrelated to the failed one that
started recovery, so the session resolves by **payment link id**, not
payment id. Matching on payment id, the obvious-looking choice, would
silently resolve nothing.

## Step 6, Metrics

```powershell
curl.exe -s http://localhost:8090/api/v1/campaigns/camp_XXXXXXXX/metrics
```

Substitute the real `campaign_id` from Step 1; a literal `<campaign_id>`
returns `campaign not found`.

```json
{"total_accounts":1,"contacted":1,
 "breakdown":{"recovered":1,"recovered_amount_paise":420000,...},
 "pending":0,"payment_links_sent":1,"razorpay_captured_confirmed":1}
```

`recovered: 0` here means Step 5 did not land.

---

## Step 7, Follow-up scheduling, and fast-forwarding the clock

This is the part that makes it an orchestration rather than a call script.
Every outcome decides **when the customer may be contacted again**:

```powershell
curl.exe -s http://localhost:8090/api/v1/campaigns/camp_XXXXXXXX/queue
```

```json
{
  "due_now": [ ... ],
  "on_hold": [
    {"customer_name":"Preethi Nair","status":"unclear","next_eligible_at":"2026-09-03T14:22:00Z"},
    {"customer_name":"Rahul Sharma","status":"link_sent","next_eligible_at":"2026-09-03T15:04:00Z"}
  ],
  "closed": [
    {"customer_name":"Vikram Rao","status":"refused","reason":"escalated to human agent"}
  ]
}
```

| Outcome | Next contact |
| ------- | ------------ |
| `UNCLEAR` | +24h |
| `AGREED`, link unpaid | +24h, *exactly when the link expires* |
| `PROMISED` | the promised date, else +24h |
| `REFUSED` | never, escalated to a human |
| Payment confirmed | never, the follow-up is cancelled |
| 3 attempts used | never |

**Fast-forwarding, on camera.** A 24-hour cooldown cannot be waited out in
a demo, so move the clock instead. Stop the services, run one `UPDATE`, and
restart:

```bash
sqlite3 /e/APPLICATIONS/CODING/project/vasuli/recovery-orchestrator/vasuli.db \
  "UPDATE recovery_sessions
      SET next_eligible_at = datetime('now','-1 hour')
    WHERE status IN ('unclear','promised','link_sent');"
```

Re-check the queue: everyone on hold is now `due_now`, with `attempt`
incremented. This is honest, no outcome is fabricated, only a timestamp
moved, and the audit trail still shows exactly what really happened.

**Worth saying while you do it:** that `UPDATE` deliberately touches every
row, including escalated and recovered ones. They stay `closed` regardless,
because eligibility checks status as well as time. A single hand-edit
cannot re-contact someone the system promised never to call again.

---

## Optional: inbound `payment.failed`

Demonstrates that a real Razorpay event can seed recovery, not just a
manually loaded batch. Requires an **active campaign** to attach to.

```bash
SECRET=whsec_vasuli_demo
BODY='{"event":"payment.failed","payload":{"payment":{"entity":{"id":"pay_inbound_demo","amount":189000,"currency":"INR","description":"Airtel Postpaid Bill","created_at":1786752000,"notes":{"customer_name":"Preethi Nair"}}}}}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $2}')
curl -s -X POST http://localhost:8090/webhooks/razorpay -H "Content-Type: application/json" -H "X-Razorpay-Signature: $SIG" -d "$BODY"
```

Preethi Nair is now queued and will be assigned to the next call.

---

## Timing targets

| Moment                                   | Target            |
| ---------------------------------------- | ----------------- |
| Start Call → Priya's first word          | < 3s (warm model) |
| Utterance ends → Priya starts responding | < 2s              |
| Hang up → outcome in `audit_log`         | < 5s              |

Measured on RTX 3050 Mobile 4GB / Ryzen 5 6600H: STT ~0.65s, LLM TTFT
~0.45s, classification ~0.75s, full teardown ~1s.

---

## When something breaks

**Browser connects, nothing happens at all.** A service is down. Run
Step 0. Terminals can show stale scrollback after a process exits.

**Nothing detected after the first exchange.** You spoke while Priya's
audio was still playing. Wait for her to finish, then leave a beat.

**Priya uses a generic greeting, no customer name.** Queue was empty,
Terminal 3 says `recovery queue empty, using static profile 'Priya'`.
Re-run Step 1.

**Priya is a dental receptionist.** Gateway started with
`-profile receptionist`. Restart with `-profile recovery_agent`.

**Outcome is REFUSED or UNCLEAR after a clear agreement.** Almost always
the closing phrase, see Step 2. A final turn containing "no" (as in "no
thanks") flips the classification to `REFUSED`. The system degrades safely:
no payment link is created on a call that was not clearly an agreement.
Manual override:

```bash
curl -s -X POST http://localhost:8090/api/v1/calls/<call_session_id>/end -H "Content-Type: application/json" -d '{"outcome":"AGREED"}'
```

**Webhook returns 400.** The secret in your shell does not match
`RAZORPAY_WEBHOOK_SECRET`, or the body changed after signing.

**Webhook says `unknown link` with a blank id.** `LINK` resolved empty,
wrong DB path, or no AGREED call yet. The guard in Step 5 catches this.

**Payment link creation fails.** Terminal 1 carries Razorpay's own error
description. Confirm the key starts `rzp_test_`.
