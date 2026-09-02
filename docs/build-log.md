# Build Log

> To make this project i followed scrum approach where each of the milestone was broken down into tasks and completed iteratively. the roadmap of milestones and tasks was documented in [`roadmap.md`](roadmap.md).

A dated, honest record of what broke while building Vasuli, why, and how it
got fixed. This is not a polished retelling - entries are written close to
when the thing actually happened, in the order it happened, including the
false alarms and the instructions that turned out to be wrong.

This is distinct from the other docs in this folder: `architecture.md`
describes the system as it is now, `roadmap.md` describes what's planned.
Neither tells you what actually went wrong on the way there. This does.

Format per entry: what we were doing, what broke (or looked broken), the
actual root cause once found, the fix, and - where there is one - the
lesson that changes how something gets done afterward.

---

## 2026-08-23 - M2: Recovery Orchestrator built and verified

**What we were doing:** First implementation of `recovery-orchestrator` -
schema, store layer, campaign manager, stub Razorpay client, HTTP handlers.
Verified with a live curl walkthrough: create campaign → assign → end call
(AGREED) → check metrics → check audit trail.

**What looked broken:** The rendered system prompt, piped through
`python -m json.tool` in this Windows/Git-Bash terminal, showed the rupee
symbol as `â‚¹4,200` instead of a clean `₹4,200` or `₹`.
That's not what a correctly-encoded UTF-8 rupee sign should ever produce
under Python's `ensure_ascii=True` escaping - a real ₹ becomes exactly one
`₹` escape, not three separate ones.

**Root cause:** None, in the code. `xxd` on the raw HTTP response bytes
showed `e2 82 b9` at the exact offset - genuinely correct UTF-8 for ₹. The
three-escape mangling was the terminal pipe re-decoding those already-UTF-8
bytes as Latin-1 before handing them to `python -m json.tool`, which then
faithfully re-escaped the _already-corrupted_ three-character string. A
display artifact one layer above the server, not a server bug.

**Fix:** None needed. Verified via `xxd` instead of trusting rendered
terminal output.

**Lesson:** When something non-ASCII looks wrong in a terminal, check the
raw bytes before assuming the code is broken. A pipe or terminal encoding
mismatch produces exactly this kind of "corrupted looking but actually
fine" output, and it's a five-second check to rule out before going
anywhere near the source.

---

## 2026-08-24 - M3: wiring var-thon to Recovery Orchestrator

### Caught during design, before it ran

Two mistakes existed in the original wiring sketch (`docs/idea_new.md`,
written before either service existed) that were caught while writing the
real code rather than while debugging a live failure:

1. The sketch set `AgentProfile.Name = ctx.CustomerName`. `Name` becomes
   `agent_name` on the wire and in Python's logs (`Profile received -
agent: 'X'`) - setting it to the customer's name would have logged the
   _caller_ as the _agent_. Only `SystemPrompt` is genuinely per-customer;
   `Name`/`Description` stay with the YAML profile.
2. The sketch's `EndSession` call had no explicit context handling, which
   in Go means it would have inherited `stream.Context()` - the gRPC
   stream's own context. `EndSession` fires exactly while that stream is
   tearing down, so that context is cancelled at essentially the same
   moment the HTTP call would try to use it. Every outcome report would
   have silently failed to leave the process, with no error visible
   anywhere obvious - the kind of bug that only surfaces days later as "why
   does the audit trail have gaps." Fixed by deriving the request context
   from `context.Background()` instead.

**Lesson:** A design doc written before the dependent service exists is a
sketch, not a spec - it should be checked against the real, now-existing
code it's supposed to integrate with, not implemented literally. Both of
these were structural, not typos; neither would have thrown a compile
error, and both would have shipped a call that _looked_ like it worked.

### The Tier 1 smoke test that looked broken

**What we were doing:** First live test of the wiring - `recovery-orchestrator`

- `inference-py` + `gateway-server -recovery ...`, probed with
  `gateway-test-client` (a synthetic-audio harness, no browser).

**What broke:** `POST /api/v1/campaigns` returned `{"error":"invalid
request body"}`. Every subsequent step then logged "recovery queue empty" -
correct behavior given no campaign existed, but not what we were trying to
verify.

**Root cause:** The curl command handed to the user used bash-style
`\"` escaping inside PowerShell, which does not consume backslash-escapes
the same way - the JSON arrived malformed and never decoded. A test
_instruction_ bug, not a bug in `recovery-orchestrator` or `var-thon`.

**Compounding issue found while investigating:** Even with the campaign
created correctly, `gateway-test-client` sends its whole (1-second, silent)
WAV file and immediately closes the stream - the entire round trip
completes in well under Python's first read-loop iteration. Timestamps
across the three terminals confirmed it: everything logged at the same
second. This means Tier 1, even fixed, can only prove the Go-side wiring
(`gateway-server` ↔ `recovery-orchestrator` HTTP calls) - it cannot prove
that a dynamically-fetched system prompt actually reaches the LLM, because
Python never gets far enough into the stream to act on it before the
connection is gone.

**Fix:** Two changes. `POST /api/v1/campaigns` now takes its JSON body from
a fixture file (`curl -d @testdata/....json`) instead of an inline string -
eliminates the shell-escaping problem entirely, for any shell. And Tier 1's
actual scope got redefined honestly: it verifies wiring only, not prompt
delivery. Prompt delivery requires Tier 2 (real browser + AetherRTC), where
the connection stays open long enough for Python to genuinely process a
session.

**Lesson:** A test that runs and passes doesn't automatically mean it
verified what you think it verified. Tier 1's silence-file design was
useful for exactly one thing (fast wiring iteration without a browser) and
useless for the thing it was almost mistaken for testing (does the AI
actually get the right prompt). Knowing precisely what a test does and
doesn't prove matters as much as the test passing.

### Tier 2 also "failed" the first time - for a completely different reason

**What we were doing:** Following up the Tier 1 fix by running the real
browser test (Tier 2) against the same `vasuli.db`, immediately after
running Tier 1 against it.

**What broke:** The agent introduced itself as Sarah from a dental clinic -
the exact same symptom as if the wiring had never worked at all.

**Root cause:** Not a repeat of the earlier bug. `testdata/m3_smoke_test.json`
seeds exactly one account. The Tier 1 run immediately before had already
popped that one account off the queue and bound it to the synthetic test
session. By the time the browser call reached `AssignSession`, the queue
was genuinely empty - `recovery-orchestrator` correctly returned 404, and
`gateway-server` correctly fell back to the static profile. Every
component behaved exactly as designed; the failure was entirely in the
test plan, which used a two-tier test against a one-account fixture
without accounting for Tier 1 consuming the only thing Tier 2 needed.

**Fix:** Delete `vasuli.db` (or just re-run the campaign-load curl) between
tiers, so each gets its own pending account.

**Result, once the queue actually had someone in it:** Full success.
Session `session_8444`, real browser audio - Priya introduced herself by
name, confirmed she was speaking with Rahul Sharma, cited the exact
outstanding amount (₹4,200), product (Bajaj Finance Personal Loan), and
due date (August 15, 2026) - all pulled from the dynamically rendered
system prompt, not hardcoded anywhere. Full audit trail confirmed:
`session_created` → `session_assigned` → `call_ended{UNCLEAR}` →
`outcome_classified{UNCLEAR}`. See `docs/roadmap.md` M3 for the full
verification log and measured latencies. **M3 is done.**

**Lesson:** Two bugs in a row now have been "the code is fine, the test
plan wasn't." Worth naming as a pattern: a shared, persistent SQLite file
across test runs means state from one test silently becomes an input to
the next one. Either reset the database between distinct test phases, or
design fixtures with enough headroom (more than one account) that an
earlier phase can't starve a later one without anyone noticing until the
symptom looks identical to a real failure.

---

## 2026-08-26 - M5: outcome classification, and three cycles chasing a

## bug that did not exist

### Two context bugs caught before they ran

M5 needed the inference engine to classify a call's outcome _after_ the
caller hangs up, then send it back. Tracing the teardown path first
turned up two things that would have made that impossible:

1. `agentCtx` derived from `stream.Context()`. gRPC cancels that context
   the instant the browser disconnects, and cancellation cascades to
   children - so the engine would be killed before it could classify,
   no matter what the surrounding code did. Fixed by rooting at
   `context.Background()`.
2. `CloseSend()` would race the inbound relay's `SendAudio()`. A gRPC
   stream permits exactly one sender; concurrent sends corrupt it or
   panic. Only reachable on the engine-ends-first path, so testing would
   likely never have caught it. Fixed by gating the half-close on proof
   the relay has exited.

Both are the same shape as the `EndSession` context bug from M3: **the
default instinct "derive from the parent context" is correct for
propagating cancellation, and wrong whenever a child must deliberately
outlive its parent.** Three instances in this codebase now. Worth
treating as a checklist item rather than rediscovering each time.

### "VAD stopped detecting" - three debugging cycles, no bug

**What broke:** After the first exchange, the agent stopped hearing
anything. Then, after a change, exactly two exchanges. It looked
identical to a freeze bug hit previously during AetherRTC integration,
where a blocking channel send deadlocked the capture goroutine.

**What it actually was:** nothing. AetherRTC gates the caller's
microphone while the agent speaks (`AgentSpeaking`, an echo-suppression
measure) using a bare `continue` - dropping packets _before_ the channel
send, leaving no trace in any log, in any service. Meanwhile Python
logged `Utterance response complete` when _synthesis_ finished, which is
several seconds before _playback_ finishes. On one run that gap was 22
seconds. Every "VAD is broken" report was speech into a gate that was
correctly closed, with no instrument anywhere showing it.

**How it was settled:** arithmetic, not intuition. Piper emits 22050Hz
16-bit mono, so `(wav_bytes - 44) / 2 / 22050` gives real playback
seconds. Applied to the successful M3 run, every utterance landed
1–2 seconds _after_ its predecessor's playback would have ended. Applied
to the failing runs, the "missing" utterances landed inside the window.
The final verification run confirmed it six for six.

**Fix:** the log line now reports playback duration, not just
completion - `4.79s of audio queued; caller audio is gated at the edge
until playback ends`. The gate itself is unchanged and correct.

**Lessons, two of them:**

- _A log line that reports the wrong milestone is worse than no log
  line._ `Utterance response complete` was true (synthesis had
  completed) and actively misleading (the turn had not). It cost three
  cycles and nearly cost an unnecessary change to a frozen repo.
- _Reaching for a remembered bug is a trap._ A previous freeze in this
  exact subsystem made "the capture goroutine is deadlocked again" the
  obvious explanation, and the first instinct was to re-apply that fix.
  What settled it was checking the actual file (the old bug was not
  present, the repo was clean) and then computing what the timings
  _should_ be. Symptoms that resemble a past bug deserve more
  verification than novel ones, not less.

### What M5 landed

Half-close teardown (`CloseSend` instead of cancellation), which also
closed the limitation tracked in `var-thon/docs/backlog.md` - _"`_read_pump`
does not currently support half-close"_ - and silenced the `grpc.RpcError`
traceback that had been logged on every normal call teardown since M3.

Verified live, `session_66867`: six exchanges, `AGREED` classified in
0.858s, payment link created, complete audit trail. See `docs/roadmap.md`
M5 for the verification log.

### Deferred with eyes open (M5)

Speech still accumulating in the VAD detector when a caller disconnects
is discarded rather than flushed into classification. The failure mode
this accepts, stated plainly: a customer who agrees and hangs up within
VAD's 800ms silence threshold leaves that agreement unclassified, and the
call resolves `UNCLEAR` instead of `AGREED`. Flushing costs ~0.7s of STT
at teardown and is a small change if rehearsal shows it matters. Logged
explicitly when it happens, so the evidence exists either way.

### Prompt tuning, stopped deliberately

Priya's replies were running 19–23 seconds of synthesized audio per turn,
and the microphone is gated for that entire duration - a long reply is not
merely verbose, it locks the customer out of the conversation. A revised
system prompt (hard word cap, one objective per turn, explicit "this is
spoken aloud") cut that to ~6s per turn, measured over five simulated
turns: **64s across 3 turns became 29.5s across 5.**

That work was then reverted, deliberately. Two reasons, both sound:

1. Tightening the prompt to fix response length started to distort other
   behaviour, and each fix needed a fresh round of live testing to
   validate.
2. Outcome classification was also failing on the same class of problem -
   with the deadline close, spending hours coaxing better instruction
   following out of a 3B model is a poor use of the remaining time
   compared to finishing Razorpay integration and rehearsal.

Worth recording precisely what was measured, because the honest version
of this is more defensible than the easy version. The classifier returned
`REFUSED` three times out of three for a conversation where the customer
said _"please give me the payment link"_ - the model anchors on the final
user turn (a polite "no thanks, bye") and discards earlier agreement. A
prompt variant fixed exactly that case, 3/3 correct, but then
over-triggered `AGREED` on genuinely refused and genuinely unclear
conversations. So: **not purely a model-capacity limit, and not purely a
prompt problem - it is a small model failing to hold two competing
constraints at once.** A larger model would very likely hold both.

The system degrades correctly regardless: an unrecognised or low-confidence
classification falls back to `UNCLEAR`, and no payment link is created on
a call that was not clearly an agreement. Every deterministic layer -
transport, VAD, state machine, payment-link creation, audit trail - is
unaffected.

---

## 2026-08-28 - M6: Razorpay integration, and a bug found by reading docs

### The demo's final step would have silently done nothing

**What we were doing:** planning M6's webhook consumer, specifically which
database column each webhook event should match a session on.

**What was wrong:** `HandlePaymentCaptured` (written back in M2) looks up
sessions by `razorpay_payment_id` - the id of the _original failed
payment_ that triggered recovery. The assumption was that a later
`payment.captured` would carry that same id.

Checking Razorpay's actual webhook documentation before writing the
handler showed it does not. A payment made against a payment link Vasuli
generated is a **new payment** with a **new id**, unrelated to the failed
one. The correct identifier is on a different event entirely:
`payment_link.paid`, at `payload.payment_link.entity.id` - which matches
the `plink_...` value already stored in `razorpay_link_id`.

So the lookup would have matched nothing, marked nothing recovered, and
produced no error - the demo's closing step, quietly doing nothing.

**Fix:** two independent paths, matched on different columns.
`payment.captured` → `razorpay_payment_id` (the demo's simulated capture
of the original failed payment). `payment_link.paid` → `razorpay_link_id`
(a real customer paying a generated link). Verified with a webhook
carrying payment id `pay_BRANDNEW999`, a value present nowhere in the
database: it resolved correctly via the link id and marked the session
recovered.

**Lesson:** this one was found by reading vendor documentation during
design rather than by debugging a failure, and it is the cheapest bug in
this log by a wide margin. It would otherwise have surfaced during
rehearsal, or worse, in front of judges - as "nothing happens", with no
error anywhere to grep for. Integration assumptions about someone else's
API are worth ten minutes of verification before they are worth an hour
of debugging.

### Log lines that lie, again

The first working version logged `payment_link.paid confirmed recovery`
on _every_ delivery, including redeliveries that correctly changed
nothing. The database was right - one audit row, one session - but the
log claimed two recoveries.

This is the same defect as M5's `Utterance response complete`: a log line
that is technically true and practically misleading. Having just spent
three debugging cycles on exactly that, it was fixed immediately rather
than left. The handlers now report whether state actually changed, so a
duplicate reads as `duplicate delivery, ignoring`.

Webhook delivery is at-least-once by design, so redelivery is the normal
path, not an edge case: `payment.failed` also dedupes on
`razorpay_payment_id` before creating a session, or a retried delivery
would queue the same customer for a second call.

### Reviewing for the failures a second reader would look for

A second opinion flagged four risks: idempotency, state transitions,
database transactions, and missing tests. All four were fair, and one was
a defect worth more than the rest.

**`EndSession` had no guard at all.** Webhook handlers were made idempotent
in M6, and the topic was treated as closed - but the call-outcome path was
never checked. Recording AGREED twice created **two real payment links**,
two payment demands against one debt. Worse, a session already marked
`refused` could be dragged back into `link_sent`, re-engaging a customer
who had declined. That is a stopping-rule violation, and stopping rules are
an explicit judging criterion.

Fixed with an allowed-from set: an outcome may only be recorded from
`active` or `unclear`. `unclear` is included deliberately - it means the
classifier reached no verdict, so an operator correcting it by hand is a
documented action rather than a duplicate.

**The tests were verified by breaking the fix.** A regression test that
passes proves nothing on its own, so the guard was disabled and the suite
re-run: `TestEndSessionIsIdempotent` and
`TestEndSessionCannotOverrideRefusal` both failed, then passed again once
restored.

**State and audit rows now commit together.** `EndSession` previously made
four or five separate writes; a failure between them could leave a session
marked `link_sent` with no `payment_link_sent` row - the audit trail
disagreeing with the state it audits. `store.RecordOutcome` now applies the
status change and every audit row in one transaction, re-checking the
status inside it so a later write cannot overwrite a settled outcome.

The payment link is created _before_ that transaction opens, for two
reasons: a provider failure must not leave a session claiming a link that
does not exist, and a ten-second network call must never be held inside a
transaction that blocks every other writer.

Removing the six now-unused single-statement status writers took ~50 lines
of dead code with it.

### Known gap: a timeout after the provider has already succeeded

`LiveClient` gives Razorpay ten seconds. If Razorpay creates a link in
eleven, the call times out, the error propagates, and nothing is recorded -
but **the link exists on Razorpay's side**. A retry then creates a second
one, and the customer receives two payment demands for one debt.

Ordering the operations differently only moves the problem: recording
first means a failed call leaves a session claiming a link that was never
created. The actual fix is an idempotency key on the request, or a
reconcile-on-startup pass that asks Razorpay what exists before creating
anything.

Not built, deliberately. Both options need more care than the remaining
schedule allows, and the failure requires a timeout landing in a window
that local test-mode calls complete in well under a second. Recorded here
rather than discovered later: knowing exactly where a system breaks is
worth more than assuming it does not.

The same window exists for two genuinely concurrent end-of-call requests -
both could create a link before either commits. The database stays
consistent (the second write is rejected), but two links would exist. In
this system `EndSession` is invoked once per session from a single
goroutine in `gateway/server.go`, so the path is unreachable short of
racing two requests by hand.

### The classifier's actual trigger, measured

The outcome classifier was known to over-weight the final turn. Testing
narrowed that to something far more specific and far more actionable:

```
conversation ending on agreement    -> AGREED   3/3
conversation ending "Thank you."    -> AGREED   3/3
conversation ending "No thanks, bye." -> REFUSED 3/3
```

It is not the sign-off. It is the word **"no"** appearing in the closing
turn, which the model reads as a refusal of the payment rather than of
further help. A polite "thank you" classifies correctly every time.

That makes it a zero-code fix for the demo - end the call without a
"no" - and a much more precise limitation to describe than "the
classifier is unreliable".

### Refusing to start on live credentials

`buildRazorpayClient` rejects any key without an `rzp_test_` prefix.
Vasuli generates payment demands autonomously from an AI conversation;
the gap between test and live mode is one CLI flag, and the failure mode
of getting it wrong is real payment requests to real people. That is
worth enforcing in code rather than trusting to whoever types the command.

---

## 2026-09-02, M7: the queue learns to wait

### The half of the workflow that was missing

The assumption going in was that customers needed removing from the queue
after a call. They did not: `AssignNextPending` only ever selected
`status = 'pending'`, and no outcome leaves a session in that state, so a
called customer could never be picked twice. That part was already
automatic.

The actual gap was the opposite. **Nobody ever came back.** Every outcome
was a one-way door, which is wrong for the case that matters most: a
customer who asks for two days should be called _in two days_, not
abandoned. A promise nobody follows up on is not a recovery workflow, it
is a single call with extra steps. The `cooldown = 24h` rule described in
`architecture.md` since M2 had never been implemented either.

So eligibility stopped being a status check and became a status _and time_
check, with `next_eligible_at` written at the moment each outcome is
recorded.

### The cooldown is load-bearing

24 hours is not a round number picked for feel. It equals
`razorpay.linkValidity`, the lifetime of a generated payment link. That
makes the follow-up land exactly when the old link dies, so calling back an
`AGREED` customer who has not paid is not nagging someone holding a working
link. The link is gone, and the call exists to issue a new one. The
constant carries a comment saying changing one without the other breaks the
property.

### NULL means never, and that direction was chosen deliberately

`next_eligible_at IS NULL` means no further contact: recovered, escalated,
or out of attempts. The alternative, defaulting to "now" and marking
terminal states some other way, fails in the dangerous direction. A code
path that forgets to set the column would contact someone forever. With
NULL as the default meaning, forgetting leaves a customer uncontacted:
recoverable, and visible in the queue view as `closed`.

### Two locks on the one door that matters

The eligibility predicate checks status _as well as_ the timestamp, which
is redundant when every write path is correct. It is kept because the demo
edits this column by hand to fast-forward a cooldown, and that `UPDATE`
touches every row in the table. Without the status check, one hand-edit
would re-contact a customer who had disputed the debt and been escalated to
a human, which is precisely the failure stopping rules exist to prevent.

Verified live rather than assumed: a blanket
`UPDATE recovery_sessions SET next_eligible_at = datetime('now','-1 hour')`
left a recovered customer sitting in `closed`, and the next assignment
returned nothing.

### "Refused" was the wrong word

The status stays `refused` in the database, but every human-facing surface
now says **escalated to a human agent**. That is what actually happens: a
disputed debt has outgrown what an automated agent should handle, so it is
handed over rather than dropped. Same data, and a materially better answer
when a judge asks what the system does with someone who says no.

### Rupees at the boundary, paise everywhere else

Fixtures now carry `"outstanding_rupees": 4200` instead of
`"outstanding_paise": 420000`, because a human writes them and the second
form is easy to get wrong by a factor of a hundred. The conversion happens
once, in the API handler. Storage and every Razorpay call stay in paise:
that is what Razorpay's API expects, and an integer count of the smallest
unit keeps floating point out of currency arithmetic entirely.

### A currency symbol that would have broken the sentence chunker

The plan was to replace `₹` with `Rs.` for clearer pronunciation. Testing
against Piper showed `Rs. 4,200` takes _longer_ to speak than
`4,200 rupees`, which suggests the abbreviation is being spelled out rather
than read as a word.

The decisive problem was elsewhere. `Rs.` ends in a period, and the LLM
sentence chunker splits on `[.!?]` past its 8-character minimum, so
`"The amount is Rs."` and `"4,200."` would have become separate TTS chunks,
putting a pause between the currency and the number. The same defect
already documented for `Mr.`. Spelling out "rupees" avoids the symbol, the
abbreviation, and the chunker, and has exactly one pronunciation.
