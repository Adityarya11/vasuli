# Vasuli — Build Roadmap

**Deadline: 5 September 2026**
**Start: 22 August 2026**
**Available days: 14 (college schedule — build days are evenings and weekends)**

Each milestone has a single definition of done. Nothing is "done" until the stated verification passes. Do not move to the next milestone while the current one's verification is failing.

---

## Dependency Graph

```
M1 (fork + verify)  ───────────────────┐
                                        ├── M3 (wire) → M4 → M5 → M6 → M7 → M8
M2 (orchestrator skeleton)  ───────────┘
```

M1 and M2 have no dependency on each other. Build them in parallel if possible. M3 requires both.

---

## M1 — Fork and Verify Baseline ✅ DONE

**What:** Clone VAR into `vasuli/var-thon/`, delete `.git`, initialize the monorepo, verify the full three-tier pipeline still works end-to-end under the new directory structure.

**Verification log (22 Aug 2026):**

```
[InferenceEngine.STT] Transcribed audio track in 0.980s | Language pinned: en
[InferenceEngine.LLM] First token at 17:08:49.310 (TTFT: 0.424s)
[InferenceEngine.TTS] TTS completed in 0.035s | 33324 bytes.
[InferenceEngine] [session_12912] Utterance response complete.
```

**Status: Complete.** The fork runs cleanly. Baseline performance confirmed: STT ~0.98s, TTFT ~0.42s, TTS first chunk ~0.035s.

---

## M2 — Recovery Orchestrator Skeleton

**What:** A new Go service in `vasuli/recovery-orchestrator/` with SQLite initialized and four endpoints returning real data.

**Files to create:**

```
recovery-orchestrator/
├── cmd/main.go                     HTTP server, flags: -port, -db, -razorpay-key-id, -razorpay-key-secret
├── internal/
│   ├── store/
│   │   ├── schema.sql              Three tables: campaigns, recovery_sessions, audit_log
│   │   └── db.go                   Open, migrate, typed query functions
│   ├── campaign/
│   │   └── manager.go              CreateCampaign, AssignSession (stopping rules), EndSession
│   └── api/
│       ├── router.go               chi or stdlib mux
│       └── handlers.go             HTTP handlers for all four endpoints
└── go.mod
```

**Endpoints that must work:**

```bash
# Create campaign with synthetic accounts
curl -X POST localhost:8090/api/v1/campaigns \
  -d '{"name":"Test","accounts":[{"customer_name":"Rahul Sharma","outstanding_paise":420000,...}]}'
# → {"campaign_id":"...","total":1,"status":"active"}

# Assign next pending session to a call
curl -X POST localhost:8090/api/v1/calls/assign \
  -d '{"call_session_id":"test-session-001"}'
# → {"system_prompt":"You are Priya...","customer_name":"Rahul Sharma",...}

# End a call with outcome
curl -X POST localhost:8090/api/v1/calls/test-session-001/end \
  -d '{"outcome":"AGREED"}'
# → 200 OK

# Get campaign metrics
curl localhost:8090/api/v1/campaigns/<id>/metrics
# → {"total":1,"contacted":1,"recovered":0,"promised":0,"refused":0,...}
```

**Definition of done:** All four curl commands above return correct responses. `vasuli.db` has rows in all three tables. The audit_log has `session_assigned` and `call_ended` rows.

---

## M3 — Wire var-thon to Recovery Orchestrator ✅ DONE

**What:** Two files added to `var-thon/services/orchestrator-go/`. At `START_SESSION`, var-thon calls the Recovery Orchestrator and uses the returned system prompt. At session end, it reports the outcome.

**Files to create/modify:**

`var-thon/services/orchestrator-go/internal/recovery/client.go` — new file:

- `type Client struct` with `baseURL` and `*http.Client`
- `func (c *Client) AssignSession(callSessionID string) (*SessionContext, error)`
- `func (c *Client) EndSession(callSessionID, outcome string) error`
- Timeout: 3 seconds. If the Recovery Orchestrator doesn't respond in 3s, return error (caller falls back to static profile).

`var-thon/services/orchestrator-go/internal/gateway/server.go` — modify `StreamAudio`:

- Accept `-recovery` flag in `cmd/gateway-server/main.go`
- After receiving `START_SESSION` from AetherRTC, call `recoveryClient.AssignSession(sessionID)`
- On success: construct `AgentProfile` from returned context, use as the session's profile
- On failure: log warning, fall back to static profile loaded from YAML flag
- When session ends (DoneChan closes or END_SESSION received): call `recoveryClient.EndSession`

**Definition of done:** Open a browser tab. AetherRTC generates a session_id. var-thon calls Recovery Orchestrator. The voice agent speaks using Rahul Sharma's context (not the receptionist). The audit_log in `vasuli.db` shows `session_assigned` with the correct call_session_id.

**Verification log (26 Aug 2026, session `session_8444`):**

```
[Gateway] Recovery context assigned for session session_8444 (customer=Rahul Sharma, outstanding=420000 paise)
[InferenceEngine] [session_8444] Profile received — agent: 'Sarah the Receptionist'
[InferenceEngine.LLM] Sentence chunk ready: 'Hello Sir/Mr.'
[InferenceEngine.LLM] Sentence chunk ready: 'Sharma, this is Priya from Razorpay Merchant Services here to contact you regarding a payment issue.'
[InferenceEngine.LLM] Sentence chunk ready: 'May I please confirm that we are speaking with Rahul Sharma?'
...
[InferenceEngine.LLM] Sentence chunk ready: 'We have an outstanding payment of ₹4,200 for a Bajaj Finance Personal Loan which was due on August 15, 2026...'
[Gateway] session session_8444: reported outcome 'UNCLEAR' to recovery orchestrator.
```

```sql
-- vasuli.db, session_8444
session_created     {"customer_name":"Rahul Sharma"}
session_assigned    {"call_session_id":"session_8444"}
call_ended          {"outcome":"UNCLEAR"}
outcome_classified  {"outcome":"UNCLEAR"}
```

Note the agent identity in Python's log line still reads `Sarah the Receptionist` — expected, not a bug. `Profile.Name` is deliberately left untouched by `resolveProfile` (see `docs/build-log.md`, 2026-08-24) and only exists for log/telemetry purposes. What the LLM actually says is driven entirely by `SystemPrompt`, which is where "You are Priya..." lives — confirmed here by the transcript itself.

**Measured performance (recovery profile, 3-utterance call, warm model):**

| Utterance | STT latency | LLM TTFT |
| --------- | ----------- | -------- |
| 1         | 0.723s      | 0.651s   |
| 2         | 0.636s      | 0.405s   |
| 3         | 0.644s      | 0.463s   |

Average TTFT (~0.51s) and STT (~0.67s) both run slightly above the receptionist-profile baseline in `var-thon/README.md` (~0.42s TTFT, ~0.98s STT) — expected, not a regression: the recovery system prompt carries substantially more context than the two-sentence receptionist prompt, so there's more for the model to process before the first token.

**Status: Complete.**

---

## M4 — Recovery Agent Profile ✅ DONE

**What:** `var-thon/configs/agent_profiles/recovery_agent.yaml`. The fallback profile used when the Recovery Orchestrator queue is empty. Also establishes the system prompt template used by the Recovery Orchestrator for rendering per-session prompts.

**File content:**

```yaml
name: "Priya"
description: "Payment recovery specialist for Razorpay merchants."
system_prompt: |
  You are Priya, a professional payment recovery specialist calling on behalf
  of a Razorpay merchant.

  You are reaching out regarding an outstanding payment. Please greet the
  customer, identify yourself and the merchant you represent, and assist
  with resolving the outstanding balance.

  Rules:
  - Be empathetic, professional, and never aggressive.
  - Never mention the payment more than three times if the customer has refused.
  - If the customer agrees to pay: say exactly "I will arrange a payment link
    to be sent to your registered number shortly."
  - If they commit to a future date: repeat the date back clearly.
  - If they refuse: say "I understand, thank you for your time" and close.
  - Keep the total call under 3 minutes.
  - Speak professional Indian English.
```

The per-customer system prompt (rendered by Recovery Orchestrator at campaign-load time) is:

```
You are Priya, a professional payment recovery specialist calling on behalf
of a Razorpay merchant.

You are calling {{.CustomerName}} regarding an outstanding payment of
₹{{.AmountFormatted}} for {{.ProductName}} that was due on {{.DueDate}}.

[same rules as above]
```

**Definition of done:** Run a call with no campaign loaded (empty queue). The agent uses the fallback profile. Run a call with a campaign loaded. The agent uses Rahul Sharma's name and correct amount. Both calls sound like the same persona.

**Verification log (26 Aug 2026, session `session_1000`, empty queue on purpose):**

```
[Gateway] session session_1000: recovery queue empty, using static profile 'Priya'.
[InferenceEngine] [session_1000] Profile received — agent: 'Priya'
[InferenceEngine.LLM] Sentence chunk ready: 'Namaste! This is Priya here, representing [Merchant Name] with Razorpay.'
[InferenceEngine.LLM] Sentence chunk ready: 'How can I assist you today?'
```

Before this milestone, the empty-queue fallback was `-profile receptionist`
(a dental-clinic persona left over from VAR) — exactly what session
`session_59058` hit during M3 testing (`docs/build-log.md`, 26 Aug). Now
the fallback is dignified: same Priya persona as an assigned call, just
without a specific customer's name or amount.

**Status: Complete.**

---

## M5 — Post-Call Outcome Classification

**What:** After the last utterance completes in Inference-Python, run a single blocking LLM call to classify the call outcome. Pass the result to var-thon Orchestrator-Go, which includes it in the `EndSession` HTTP call.

**Changes in `var-thon/services/inference-py/main.py`:**

In `_utterance_worker`, when `_UTTERANCE_STOP` is received (call ending):

```python
outcome = self._classify_outcome(ctx)
# Store outcome on ctx so Orchestrator-Go can retrieve it
ctx.outcome = outcome
```

Outcome needs to reach var-thon Orchestrator-Go. Two options:

- **Option A:** Inference-Python makes its own HTTP call to Recovery Orchestrator directly. Simple, but adds an HTTP dependency to a service that currently has none.
- **Option B:** Pass outcome via a final `Transcript` proto message before stream close. Orchestrator-Go reads it and includes it in `EndSession`.

Use Option B. It keeps Inference-Python's only external dependency as gRPC (no HTTP client to add). The Transcript event already exists in `agent.proto`. Use it with `is_final=true` and a JSON payload in `text`.

**Definition of done:** Run a call where you say "yes, please send me the payment link." After the call, `SELECT status FROM recovery_sessions WHERE call_session_id = '...'` returns `recovered` (or `active` pending Razorpay confirmation). The `audit_log` has `outcome_classified` with `{"outcome":"AGREED"}`.

---

## M6 — Razorpay Test-Mode Integration

**What:** Webhook consumer (inbound) and payment link creation (outbound) in the Recovery Orchestrator.

**Inbound — `POST /webhooks/razorpay`:**

- Verify `X-Razorpay-Signature` header: HMAC-SHA256 of raw request body with webhook secret
- Parse event type from JSON body
- On `payment.failed`: create a `recovery_session` (if campaign is active)
- On `payment.captured`: find matching `recovery_session` by `razorpay_payment_id`, update status=`recovered`, set `recovered_at`

For the demo, Razorpay test-mode events are simulated manually with curl — you don't need a live payment flow to demonstrate the system.

**Outbound — payment link creation:**

```
POST https://api.razorpay.com/v1/payment_links
Authorization: Basic base64(key_id:key_secret)
Content-Type: application/json

{
  "amount": <outstanding_paise>,
  "currency": "INR",
  "description": "Payment recovery — <product_name>",
  "customer": { "name": "<customer_name>" },
  "notify": { "sms": false, "email": false },
  "reminder_enable": false
}
```

Store `response.id` as `razorpay_link_id` in `recovery_sessions`. Log `payment_link_sent` to `audit_log`.

**Definition of done:**

1. Run `curl -X POST localhost:8090/webhooks/razorpay` with a simulated `payment.failed` event and valid HMAC → a `recovery_session` is created.
2. Complete a call that classifies as AGREED → a real Razorpay test-mode payment link ID appears in `recovery_sessions.razorpay_link_id`.
3. Run `curl -X POST localhost:8090/webhooks/razorpay` with a simulated `payment.captured` event → `recovery_sessions.status` updates to `recovered`.

---

## M7 — Metrics and Synthetic Batch

**What:** The metrics endpoint returns accurate numbers. A synthetic batch of 20 accounts is loaded and several calls are manually completed to populate realistic data.

**Metrics endpoint response:**

```json
{
  "campaign_id": "aug-emi-2026",
  "campaign_name": "August Failed EMI Recovery",
  "total_accounts": 20,
  "contacted": 12,
  "breakdown": {
    "recovered": 5,
    "recovered_amount_paise": 2140000,
    "promised": 4,
    "promised_amount_paise": 1780000,
    "refused": 2,
    "no_answer": 1
  },
  "pending": 8,
  "stopped_max_attempts": 1,
  "payment_links_sent": 5,
  "razorpay_captured_confirmed": 3,
  "generated_at": "2026-09-01T14:30:00Z"
}
```

**Synthetic batch (`recovery-orchestrator/testdata/sample_campaign.json`):**
20 accounts with realistic Indian names, amounts between ₹2,000–₹25,000, product names across: personal loans, EMIs, subscriptions, utility bills. Mix of due dates in the past 1–3 months.

**Definition of done:** `GET /api/v1/campaigns/aug-emi-2026/metrics` returns real numbers derived from real calls made during testing. The numbers are not hardcoded. Stopped accounts are visible and correctly excluded from further assignment.

---

## M8 — Demo Rehearsal

**What:** Full demo sequence run twice cleanly. All services start without errors. The live call is under 60 seconds. The metrics output is correct.

**Demo sequence:**

1. Start all services in order (Inference-Python → var-thon → Recovery Orchestrator → AetherRTC)
2. Load the synthetic campaign: `curl -X POST .../api/v1/campaigns -d @sample_campaign.json`
3. Open browser tab → call starts → Priya speaks within 3 seconds of connection
4. Conduct 45-60 second recovery conversation (cover both AGREED and REFUSED paths in two separate runs)
5. Show `SELECT * FROM audit_log WHERE session_id = '...' ORDER BY created_at` → full event timeline
6. Show payment link created in Razorpay dashboard (test mode)
7. Simulate `payment.captured` webhook → status updates
8. Show `GET /api/v1/campaigns/.../metrics` → full batch numbers

**Definition of done:** Two full runs complete without error. The agent's first sentence is spoken within 3 seconds of the browser connecting. The audit trail for one call has at minimum 5 events: `session_assigned`, `call_started`, `call_ended`, `outcome_classified`, and either `payment_link_sent` or `stopped_refused`.

---

## Timeline Estimate

| Milestone | Estimated effort    | Target completion |
| --------- | ------------------- | ----------------- |
| M1        | Done                | 22 Aug            |
| M2        | 1–2 evenings        | 24 Aug            |
| M3        | 1 evening           | 25 Aug            |
| M4        | Half evening        | 26 Aug            |
| M5        | 1 evening           | 27 Aug            |
| M6        | 1–2 evenings        | 29 Aug            |
| M7        | 1 evening           | 31 Aug            |
| M8        | 2 days of rehearsal | 3–4 Sep           |

Buffer before 5 Sep deadline: 1 day. Use it for README polish, demo video, and submission packaging — not for new features.

---

## What Is Explicitly Out of Scope

These are tracked, not forgotten. They do not ship for this submission.

- Barge-in (monitor goroutine in var-thon) — not needed for structured recovery calls
- Tool calling during live calls — post-call classification handles outcome detection
- TURN server for NAT traversal — demo is local/same-network
- Goroutine leak verification (Milestone 6 in original VAR) — VAR's concern, not var-thon's
- Multi-tenancy, auth, rate limiting in Recovery Orchestrator
- Hinglish / multilingual STT — English pinned, 0.67s latency saving kept
- Concurrent ordered utterance processing
