package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	agentpb "voice-runtime/orchestrator-go/generated"
	gatewaypb "voice-runtime/orchestrator-go/generated/gateway"
	"voice-runtime/orchestrator-go/internal/config"
	"voice-runtime/orchestrator-go/internal/recovery"
	"voice-runtime/orchestrator-go/internal/session"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fallbackOutcome is reported when a call ends without a classified
// outcome. UNCLEAR is one of the four outcomes the Recovery Orchestrator
// already understands and is the honest description of a call whose
// transcript has not been classified yet, so it needs no rework once
// post-call classification lands — that milestone only has to replace this
// constant with the classifier's verdict.
const fallbackOutcome = "UNCLEAR"

type Server struct {
	gatewaypb.UnimplementedGatewayServer

	// Profile is the static fallback persona loaded at startup. It is read
	// concurrently by every session and never mutated — per-call personas
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
// It reports whether a recovery session was actually assigned — only an
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
	// through unchanged — overwriting Name with the customer's name would
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
		return fmt.Errorf("gateway: failed to recieve initial event: %v", err)
	}

	control := first.GetControl()
	if control == nil || control.Type != gatewaypb.GatewayControl_START_SESSION {
		return fmt.Errorf("gateway: expected START_SESSION as first event; got %T", first.Payload)
	}

	sessionID := first.SessionId
	sourceSampleRate := control.SourceSampleRate

	log.Printf("[Gateway] Incoming session %s, source_sample_rate=%d", sessionID, sourceSampleRate)

	agentClient := agentpb.NewVoiceAgentClient(s.conn)
	agentCtx, cancelAgent := context.WithCancel(stream.Context())
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
		for chunk := range sess.AgentAudioChan {
			err := stream.Send(&gatewaypb.GatewayEvent{
				SessionId: sessionID,
				Payload: &gatewaypb.GatewayEvent_Audio{
					Audio: &gatewaypb.AudioChunk{
						Data: chunk,
					},
				},
			})

			if err != nil {
				log.Printf("[Gateway] session %s: outbound send to AetherRTC failed: %v", sessionID, err)
				return
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

	select {
	case <-sess.DoneChan:
		log.Printf("[Gateway] session %s: Python side ended.", sessionID)
	case err := <-inboundErr:
		if err == io.EOF {
			log.Printf("[Gateway] session %s: AetherRTC closed stream.", sessionID)
		} else {
			log.Printf("[Gateway] session %s: AetherRTC recv error: %v", sessionID, err)
		}
	}

	cancelAgent()
	<-outboundDone

	if recoveryAssigned {
		if err := s.RecoveryClient.EndSession(sessionID, fallbackOutcome, ""); err != nil {
			log.Printf("[Gateway] session %s: failed to report call outcome: %v", sessionID, err)
		} else {
			log.Printf("[Gateway] session %s: reported outcome '%s' to recovery orchestrator.",
				sessionID, fallbackOutcome)
		}
	}

	return nil

}
