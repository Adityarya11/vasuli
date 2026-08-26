# Build Log

A dated, honest record of what broke while building Vasuli, why, and how it
got fixed. This is not a polished retelling — entries are written close to
when the thing actually happened, in the order it happened, including the
false alarms and the instructions that turned out to be wrong.

This is distinct from the other docs in this folder: `architecture.md`
describes the system as it is now, `roadmap.md` describes what's planned.
Neither tells you what actually went wrong on the way there. This does.

Format per entry: what we were doing, what broke (or looked broken), the
actual root cause once found, the fix, and — where there is one — the
lesson that changes how something gets done afterward.

---

## 2026-08-23 — M2: Recovery Orchestrator built and verified

**What we were doing:** First implementation of `recovery-orchestrator` —
schema, store layer, campaign manager, stub Razorpay client, HTTP handlers.
Verified with a live curl walkthrough: create campaign → assign → end call
(AGREED) → check metrics → check audit trail.

**What looked broken:** The rendered system prompt, piped through
`python -m json.tool` in this Windows/Git-Bash terminal, showed the rupee
symbol as `â‚¹4,200` instead of a clean `₹4,200` or `₹`.
That's not what a correctly-encoded UTF-8 rupee sign should ever produce
under Python's `ensure_ascii=True` escaping — a real ₹ becomes exactly one
`₹` escape, not three separate ones.

**Root cause:** None, in the code. `xxd` on the raw HTTP response bytes
showed `e2 82 b9` at the exact offset — genuinely correct UTF-8 for ₹. The
three-escape mangling was the terminal pipe re-decoding those already-UTF-8
bytes as Latin-1 before handing them to `python -m json.tool`, which then
faithfully re-escaped the *already-corrupted* three-character string. A
display artifact one layer above the server, not a server bug.

**Fix:** None needed. Verified via `xxd` instead of trusting rendered
terminal output.

**Lesson:** When something non-ASCII looks wrong in a terminal, check the
raw bytes before assuming the code is broken. A pipe or terminal encoding
mismatch produces exactly this kind of "corrupted looking but actually
fine" output, and it's a five-second check to rule out before going
anywhere near the source.

---

## 2026-08-24 — M3: wiring var-thon to Recovery Orchestrator

### Caught during design, before it ran

Two mistakes existed in the original wiring sketch (`docs/idea_new.md`,
written before either service existed) that were caught while writing the
real code rather than while debugging a live failure:

1. The sketch set `AgentProfile.Name = ctx.CustomerName`. `Name` becomes
   `agent_name` on the wire and in Python's logs (`Profile received —
   agent: 'X'`) — setting it to the customer's name would have logged the
   *caller* as the *agent*. Only `SystemPrompt` is genuinely per-customer;
   `Name`/`Description` stay with the YAML profile.
2. The sketch's `EndSession` call had no explicit context handling, which
   in Go means it would have inherited `stream.Context()` — the gRPC
   stream's own context. `EndSession` fires exactly while that stream is
   tearing down, so that context is cancelled at essentially the same
   moment the HTTP call would try to use it. Every outcome report would
   have silently failed to leave the process, with no error visible
   anywhere obvious — the kind of bug that only surfaces days later as "why
   does the audit trail have gaps." Fixed by deriving the request context
   from `context.Background()` instead.

**Lesson:** A design doc written before the dependent service exists is a
sketch, not a spec — it should be checked against the real, now-existing
code it's supposed to integrate with, not implemented literally. Both of
these were structural, not typos; neither would have thrown a compile
error, and both would have shipped a call that *looked* like it worked.

### The Tier 1 smoke test that looked broken

**What we were doing:** First live test of the wiring — `recovery-orchestrator`
+ `inference-py` + `gateway-server -recovery ...`, probed with
`gateway-test-client` (a synthetic-audio harness, no browser).

**What broke:** `POST /api/v1/campaigns` returned `{"error":"invalid
request body"}`. Every subsequent step then logged "recovery queue empty" —
correct behavior given no campaign existed, but not what we were trying to
verify.

**Root cause:** The curl command handed to the user used bash-style
`\"` escaping inside PowerShell, which does not consume backslash-escapes
the same way — the JSON arrived malformed and never decoded. A test
*instruction* bug, not a bug in `recovery-orchestrator` or `var-thon`.

**Compounding issue found while investigating:** Even with the campaign
created correctly, `gateway-test-client` sends its whole (1-second, silent)
WAV file and immediately closes the stream — the entire round trip
completes in well under Python's first read-loop iteration. Timestamps
across the three terminals confirmed it: everything logged at the same
second. This means Tier 1, even fixed, can only prove the Go-side wiring
(`gateway-server` ↔ `recovery-orchestrator` HTTP calls) — it cannot prove
that a dynamically-fetched system prompt actually reaches the LLM, because
Python never gets far enough into the stream to act on it before the
connection is gone.

**Fix:** Two changes. `POST /api/v1/campaigns` now takes its JSON body from
a fixture file (`curl -d @testdata/....json`) instead of an inline string —
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

### Tier 2 also "failed" the first time — for a completely different reason

**What we were doing:** Following up the Tier 1 fix by running the real
browser test (Tier 2) against the same `vasuli.db`, immediately after
running Tier 1 against it.

**What broke:** The agent introduced itself as Sarah from a dental clinic —
the exact same symptom as if the wiring had never worked at all.

**Root cause:** Not a repeat of the earlier bug. `testdata/m3_smoke_test.json`
seeds exactly one account. The Tier 1 run immediately before had already
popped that one account off the queue and bound it to the synthetic test
session. By the time the browser call reached `AssignSession`, the queue
was genuinely empty — `recovery-orchestrator` correctly returned 404, and
`gateway-server` correctly fell back to the static profile. Every
component behaved exactly as designed; the failure was entirely in the
test plan, which used a two-tier test against a one-account fixture
without accounting for Tier 1 consuming the only thing Tier 2 needed.

**Fix:** Delete `vasuli.db` (or just re-run the campaign-load curl) between
tiers, so each gets its own pending account.

**Result, once the queue actually had someone in it:** Full success.
Session `session_8444`, real browser audio — Priya introduced herself by
name, confirmed she was speaking with Rahul Sharma, cited the exact
outstanding amount (₹4,200), product (Bajaj Finance Personal Loan), and
due date (August 15, 2026) — all pulled from the dynamically rendered
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
