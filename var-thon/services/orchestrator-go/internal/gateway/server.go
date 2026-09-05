package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"
	agentpb "voice-runtime/orchestrator-go/generated"
	gatewaypb "voice-runtime/orchestrator-go/generated/gateway"
	"voice-runtime/orchestrator-go/internal/config"
	"voice-runtime/orchestrator-go/internal/recovery"
	"voice-runtime/orchestrator-go/internal/session"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fallbackOutcome is reported when a call ends without the inference engine
// delivering a classification, timeout, engine failure, or a call with no
// conversation at all. UNCLEAR is one of the four outcomes the Recovery
// Orchestrator already understands, so this degrades cleanly rather than
// inventing a verdict.
const fallbackOutcome = "UNCLEAR"

// outcomeTimeout bounds the wait for post-call classification after the
// inference stream is half-closed. Budget, from measured latencies: an
// in-flight utterance must drain first (~3.0s worst case observed), then
// the classification inference itself (~1.5s with full conversation
// history), plus transport. 5s covers that with headroom and keeps the
// whole teardown, from the caller hanging up to the outcome landing in the
// audit log, inside the 5s budget the recovery orchestrator is held to.
//
// This is now the only bound on that wait: the inference context no longer
// derives from the caller's stream, so gRPC will not cancel it for us.
const outcomeTimeout = 5 * time.Second

type Server struct {
	gatewaypb.UnimplementedGatewayServer

	// Profile is the static fallback persona loaded at startup. It is read
	// concurrently by every session and never mutated, per-call personas
	// are resolved into a local profile inside StreamAudio instead.
	Profile *config.AgentProfile

	// RecoveryClient is nil when the server runs without -recovery, in
	// which case every session uses Profile and behaves exactly as it did
	// before recovery integration existed.
	RecoveryClient *recovery.Client

	conn *grpc.ClientConn
}

func NewServer(profile *config.AgentProfile, inferenceAddr, recoveryURL string) (*Server, error) {

	conn, err := grpc.NewClient(
		inferenceAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("gateway: failed to connect to inference engine: %v", err)
	}

	var recoveryClient *recovery.Client
	if recoveryURL != "" {
		recoveryClient = recovery.NewClient(recoveryURL)
	}

	return &Server{
		Profile:        profile,
		RecoveryClient: recoveryClient,
		conn:           conn,
	}, nil
}

// resolveProfile determines which agent persona a session should run with.
// It reports whether a recovery session was actually assigned, only an
// assigned session has a binding on the orchestrator side to close out
// when the call ends.
func (s *Server) resolveProfile(sessionID string) (*config.AgentProfile, bool) {
	if s.RecoveryClient == nil {
		return s.Profile, false
	}

	sessionCtx, err := s.RecoveryClient.AssignSession(sessionID)
	if err != nil {
		if errors.Is(err, recovery.ErrNoPendingSession) {
			log.Printf("[Gateway] session %s: recovery queue empty, using static profile '%s'.",
				sessionID, s.Profile.Name)
		} else {
			log.Printf("[Gateway] session %s: recovery assign failed (%v), using static profile '%s'.",
				sessionID, err, s.Profile.Name)
		}
		return s.Profile, false
	}

	log.Printf("[Gateway] Recovery context assigned for session %s (recovery_session=%s, customer=%s, outstanding=%d paise)",
		sessionID, sessionCtx.SessionID, sessionCtx.CustomerName, sessionCtx.OutstandingAmountPaise)

	// Only the system prompt is per-customer. The agent's own identity
	// belongs to the YAML profile, so Name and Description are carried
	// through unchanged. Overwriting Name with the customer's name would
	// make the inference engine log the caller as the agent.
	return &config.AgentProfile{
		Name:         s.Profile.Name,
		Description:  s.Profile.Description,
		SystemPrompt: sessionCtx.SystemPrompt,
	}, true
}

func (s *Server) StreamAudio(stream gatewaypb.Gateway_StreamAudioServer) error {
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("gateway: failed to receive initial event: %v", err)
	}

	control := first.GetControl()
	if control == nil || control.Type != gatewaypb.GatewayControl_START_SESSION {
		return fmt.Errorf("gateway: expected START_SESSION as first event; got %T", first.Payload)
	}

	sessionID := first.SessionId
	sourceSampleRate := control.SourceSampleRate

	log.Printf("[Gateway] Incoming session %s, source_sample_rate=%d", sessionID, sourceSampleRate)

	agentClient := agentpb.NewVoiceAgentClient(s.conn)

	// Deliberately rooted at Background, not stream.Context(). The inference
	// stream must outlive the caller's connection: when the customer hangs
	// up, gRPC cancels stream.Context(), and a derived context would cascade
	// that cancellation into the engine before it can classify the call.
	// cancelAgent below is what tears this down instead, and the deferred
	// call guarantees it fires on every exit path.
	agentCtx, cancelAgent := context.WithCancel(context.Background())
	defer cancelAgent()

	agentStream, err := agentClient.StreamEvents(agentCtx)
	if err != nil {
		return fmt.Errorf("gateway: failed to open agent stream: %v", err)
	}

	profile, recoveryAssigned := s.resolveProfile(sessionID)

	sess := session.NewSession(sessionID, profile)
	if err := sess.Attach(agentStream); err != nil {
		return fmt.Errorf("gateway: failed to attach the session: %v", err)
	}

	err = agentStream.Send(&agentpb.Event{
		SessionId: sessionID,
		Payload: &agentpb.Event_Control{
			Control: &agentpb.ControlSignal{
				Type:             agentpb.ControlSignal_START_SESSION,
				SourceSampleRate: sourceSampleRate,
				Profile: &agentpb.AgentProfile{
					AgentName:    sess.Profile.Name,
					SystemPrompt: sess.Profile.SystemPrompt,
				},
			},
		},
	})

	if err != nil {
		return fmt.Errorf("gateway: failed to send START_SESSION to inference engine: %v", err)
	}

	log.Printf("[Gateway] Session %s attached to inference engine.", sessionID)

	go sess.Run()

	outboundDone := make(chan struct{})
	go func() {
		defer close(outboundDone)

		// On send failure this keeps draining AgentAudioChan instead of
		// returning. The caller is gone, so the audio has nowhere to go, but
		// abandoning the channel would let its buffer fill and block the
		// session's readPump, which still has to receive the classified
		// outcome before teardown.
		sendFailed := false

		for chunk := range sess.AgentAudioChan {
			if sendFailed {
				continue
			}

			err := stream.Send(&gatewaypb.GatewayEvent{
				SessionId: sessionID,
				Payload: &gatewaypb.GatewayEvent_Audio{
					Audio: &gatewaypb.AudioChunk{
						Data: chunk,
					},
				},
			})

			if err != nil {
				log.Printf("[Gateway] session %s: outbound send to AetherRTC failed, discarding remaining audio: %v", sessionID, err)
				sendFailed = true
			}
		}
	}()

	inboundErr := make(chan error, 1)
	go func() {
		for {
			event, err := stream.Recv()
			if err != nil {
				inboundErr <- err
				return
			}
			if audio := event.GetAudio(); audio != nil {
				if err := sess.SendAudio(audio.Data); err != nil {
					inboundErr <- err
					return
				}
				continue
			}

			if control := event.GetControl(); control != nil && control.Type == gatewaypb.GatewayControl_END_SESSION {
				inboundErr <- io.EOF
				return
			}
		}
	}()

	// inboundRelayExited records whether the goroutine above, the only
	// other caller of Send on the inference stream, has stopped. Every
	// path that publishes to inboundErr does so after its final SendAudio
	// has returned and immediately before the goroutine exits, so receiving
	// from that channel proves no further send can be in flight.
	inboundRelayExited := false

	select {
	case <-sess.DoneChan:
		log.Printf("[Gateway] session %s: Python side ended.", sessionID)
	case err := <-inboundErr:
		inboundRelayExited = true
		if err == io.EOF {
			log.Printf("[Gateway] session %s: AetherRTC closed stream.", sessionID)
		} else {
			log.Printf("[Gateway] session %s: AetherRTC recv error: %v", sessionID, err)
		}
	}

	outcome, promiseDate := s.collectOutcome(sess, sessionID, inboundRelayExited)

	cancelAgent()
	<-outboundDone

	if recoveryAssigned {
		if err := s.RecoveryClient.EndSession(sessionID, outcome, promiseDate); err != nil {
			log.Printf("[Gateway] session %s: failed to report call outcome: %v", sessionID, err)
		} else {
			log.Printf("[Gateway] session %s: reported outcome '%s' to recovery orchestrator.",
				sessionID, outcome)
		}
	}

	return nil

}

// collectOutcome half-closes the inference stream to signal that no further
// audio is coming, then waits for the engine's post-call classification.
// Falls back to fallbackOutcome rather than blocking teardown indefinitely.
//
// inboundRelayExited must be true before this half-closes: a gRPC stream
// permits only one sender at a time, and CloseSend concurrent with the
// relay's SendAudio is a data race that can corrupt the stream or panic.
func (s *Server) collectOutcome(sess *session.Session, sessionID string, inboundRelayExited bool) (string, string) {
	if outcome, promiseDate := sess.Outcome(); outcome != "" {
		return outcome, promiseDate
	}

	// Reaching here with the relay still live means the engine ended first
	// and never classified, so there is nothing a half-close could still
	// elicit, and attempting one would only risk the race.
	if !inboundRelayExited {
		log.Printf("[Gateway] session %s: inference engine ended before classifying, falling back to '%s'.",
			sessionID, fallbackOutcome)
		return fallbackOutcome, ""
	}

	if err := sess.SignalInputComplete(); err != nil {
		log.Printf("[Gateway] session %s: failed to half-close inference stream: %v", sessionID, err)
		return fallbackOutcome, ""
	}

	select {
	case <-sess.OutcomeChan:
	case <-sess.DoneChan:
		// Engine closed its side without classifying, handled below.
	case <-time.After(outcomeTimeout):
		log.Printf("[Gateway] session %s: timed out after %s waiting for outcome classification.",
			sessionID, outcomeTimeout)
	}

	outcome, promiseDate := sess.Outcome()
	if outcome == "" {
		log.Printf("[Gateway] session %s: no outcome classified, falling back to '%s'.",
			sessionID, fallbackOutcome)
		return fallbackOutcome, ""
	}

	return outcome, promiseDate
}
