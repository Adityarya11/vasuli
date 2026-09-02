# Pitch notes

Two things live here. The video script, and the answers for the submission
form. Both exist so the same claims get made the same way in both places.

---

## Video script

Read it as notes, not as a teleprompter. Where a line is worth getting
exactly right it is marked **[say this]**, and everything else is a
reminder of what to cover.

### Open, before the demo

> Hi. This is Vasuli, an AI revenue recovery system. It calls people about
> money they owe, has an actual conversation, and then does something about
> how that conversation went.
>
> I am going to run the whole thing first and explain the parts afterwards,
> so stay till the end if you want the architecture.

Keep this under twenty seconds. Nobody has decided anything about you yet,
and the demo is what decides it.

### The demo

Run it. Talk as little as possible over it.

Two moments are worth pointing at while they happen:

- The queue, before the call. **[say this]** "Nothing has happened yet, and
  the system has already decided who to call. That decision is the outbound
  part."
- The payment link appearing in the audit log. **[say this]** "That is a
  real Razorpay link. I did not create it, the conversation did."

Do not narrate the parts that speak for themselves.

### Service one: recovery-orchestrator

Show `internal/campaign/manager.go` and `internal/store/db.go` while
talking.

This is the part that knows about money. Written in Go with the standard
library HTTP package, no framework. It owns the campaign, the queue, the
stopping rules, and the audit log.

The interesting bit is not the endpoints, it is the scheduling. Every call
outcome decides when that customer may be contacted again:

- Agrees to pay, link created, twenty four hours to pay it
- Names a date, we wait for that date
- No clear answer, twenty four hours
- Disputes the debt, we never call again and a human takes the account

**[say this]** "The twenty four hour number is not arbitrary. A Razorpay
payment link expires in twenty four hours, so the follow-up lands exactly
when the old link dies. The callback exists to issue a new one, not to
nag."

Then the stopping rule, said plainly: three attempts and the account stops
being called, whatever the outcome. A recovery workflow that never
terminates is a harassment workflow.

### Service two: var-thon, the voice agent runtime

Show the README and the three-tier diagram.

**[say this]** "This one is not new. It is a project I have been building
for about three months, before this hackathon existed."

Two processes. Orchestrator-Go owns session lifecycle and a state machine
and never touches a model. Inference-Python owns the pipeline and never
touches call lifecycle. They talk over a gRPC bidirectional stream.

The pipeline: Silero VAD finds the speech boundaries, Faster-Whisper
transcribes on CPU, Qwen2.5 3B generates on the GPU, Piper synthesizes back
on CPU. The split is deliberate, since nothing competes for 4GB of VRAM.

On why it is all local. The brief mentioned a Hinglish recovery bot, and
the honest reason there is no hosted model here is that I wanted to build
the layer rather than rent it. **[say this]** "I did not want a system where
the interesting part happens inside somebody else's API. So there is no
cloud speech, no cloud LLM, no cloud TTS. It runs on this laptop."

Mention the older video for the runtime itself, link in the description.

### Service three: AetherRTC

**[say this]** "This is the WebRTC gateway. It terminates the browser
connection, forces the G.711 telephony codec so the audio path matches what
a real phone call would carry, and bridges it to the orchestrator over
gRPC. It knows nothing about AI. It moves audio and a session id."

Then the limitation, and do not soften it:

**[say this]** "The biggest gap in this system is that the agent does not
dial the customer. I opened the browser and I said the first word. After
that, everything you saw was the agent driving, the outcome being
classified, and the queue rescheduling itself with nobody in the loop.
The transport is inbound. The workflow is outbound. Making it genuinely
outbound means a telephony provider, a number, per-minute costs, and DND
compliance, and none of that would change the recovery logic. So I built
the recovery logic."

That framing is the honest one and it is stronger than pretending.

### Close

**[say this]** "All of this runs on an RTX 3050 with 4GB of VRAM. Some of
the choices here are shaped by that, and I have written down which ones and
why, in the build log in the repo. That log also has the bugs I hit and how
I found them, including one I caught by reading Razorpay's webhook
documentation before writing the handler rather than after."

Then thanks, and that it is a submission to the Razorpay AI Buildathon.

### Lines to avoid saying

- "Seamlessly" and "leverages". Nobody talks like that.
- Any claim that a limitation is actually a strength. Say the limitation,
  say what you would do about it, move on.
- Numbers you have not measured. Every figure in this script is in the
  repo.

---

## Form answers

### Project name

Vasuli

### Project objectives, and what it solves

A failed EMI payment is worth chasing. Ten thousand of them are worth
chasing too, and that is where it breaks down. Human recovery agents cost
money and have bad days. SMS and email get ignored, and neither one can
answer "can I pay on the fifth instead."

Vasuli is a voice agent that has that conversation and then acts on it. It
picks the customer from a queue it maintains, opens the call knowing the
name and the amount and the due date, negotiates, and classifies the
outcome from the transcript once the call ends. If the customer agrees, a
Razorpay payment link is created in the same breath. If they name a date,
the system waits for it and calls back. If they dispute the debt, it stops
calling permanently and the account goes to a human.

Three attempts is the hard ceiling. Every state change is written to an
append-only audit log, so a merchant can ask what happened to any account
and get an answer with timestamps.

The whole voice pipeline is self-hosted on a 4GB laptop GPU. Speech
recognition, the language model, synthesis, and the WebRTC transport are
all local. The only outbound call during a recovery is to Razorpay, to
create the link.

### Build challenges and technical obstacles

Four worth naming, all documented in full in `docs/build-log.md`.

**A bug found by reading the docs instead of by debugging.** The webhook
handler resolved a confirmed payment back to a recovery session using the
id of the payment that had originally failed. Razorpay's documentation says
that a payment made against a generated link is a *new* payment with a
different id, carried on a different event. The original lookup would have
matched nothing, marked nothing recovered, and raised no error. The demo's
closing step would have quietly done nothing. It cost ten minutes to catch
before writing the handler and would have cost far more to find during a
rehearsal.

**Three debugging cycles lost to a log line that reported the wrong
milestone.** The agent stopped hearing the caller after the first exchange.
It looked exactly like a deadlock hit previously in the same subsystem, and
the instinct was to re-apply that fix. It was not that. The gateway mutes
the caller's microphone while the agent is speaking, using a bare
`continue` that leaves no trace in any log, and the inference engine was
printing "response complete" when *synthesis* finished, several seconds
before playback did. On one run that gap was twenty two seconds. The fix
was arithmetic, not code: Piper emits 22050Hz audio, so the byte count
gives the real playback duration, and every "missing" utterance turned out
to land inside the window where the microphone was correctly closed. The
log line now reports how long the caller must wait.

**Recording an outcome twice created two real payment links.** There was no
guard on the session's current state, so a repeated call would issue a
second payment demand against one debt, and worse, a customer already
marked as disputing the debt could be dragged back into an active state and
contacted again. That is precisely the failure the stopping rules exist to
prevent. The fix constrains which states an outcome may be recorded from
and applies the state change and its audit rows in a single transaction, so
the audit trail can never disagree with the state it describes. Both
regression tests were verified by disabling the guard and confirming they
fail.

**One hole left open on purpose.** If the payment-link request to Razorpay
times out after Razorpay has already created the link, nothing records it,
and a retry creates a second one. The fix is an idempotency key on the
request. I know where the failure is and I know what closes it, and I chose
not to build it in the time available rather than build it badly. It is
written down in the build log rather than hidden.

### Known limitations

The customer joins the call rather than being dialled, because PSTN
telephony would have added cost and compliance work without changing the
recovery logic.

Outcome classification is the weakest component and the one that improves
with a larger model rather than better engineering. A call ending "thank
you" classifies correctly every time. The same call ending "no thanks, bye"
flips to a refusal, because a 3B model reads that "no" as refusing the
payment. It degrades safely, since no payment link is created on an unclear
call.

Replies are longer than they should be, and English is pinned rather than
Hinglish. Both are the same 3B constraint.

No authentication on any endpoint. It binds to localhost and is meant to.
