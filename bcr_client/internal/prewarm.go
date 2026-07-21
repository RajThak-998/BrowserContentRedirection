package engine

import (
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/ice/v4"
	"github.com/pion/stun/v3"
)

// PreWarm holds a ready-to-use ICE agent with credentials.
// Created at startup or after a call ends. Consumed when a call begins.
type PreWarm struct {
	mu              sync.Mutex
	id              string           // unique preWarmId (UUID)
	agent           *ice.Agent
	cert            tls.Certificate
	fingerprint     string
	ufrag           string
	pwd             string
	candidates      []string         // host candidates gathered so far
	createdAt       time.Time
	consumed        bool
	onLateCandidate func(string)     // set when bound to a session
	readySentCount  int              // how many candidates were in initial response
	stunURIs        []*stun.URI
}

func newPreWarm(stunURIs []*stun.URI, logf func(string, ...any)) (*PreWarm, error) {
	if globalCertErr != nil {
		return nil, fmt.Errorf("pre-generated global cert failed: %w", globalCertErr)
	}
	cert := globalCert
	fingerprint := globalCertFingerprint

	agentConfig := &ice.AgentConfig{
		Urls: stunURIs,
		NetworkTypes: []ice.NetworkType{
			ice.NetworkTypeUDP4,
			ice.NetworkTypeTCP4,
		},
	}

	agent, err := ice.NewAgent(agentConfig)
	if err != nil {
		return nil, fmt.Errorf("new ice agent: %w", err)
	}

	ufrag, pwd, err := agent.GetLocalUserCredentials()
	if err != nil {
		agent.Close()
		return nil, fmt.Errorf("GetLocalUserCredentials: %w", err)
	}

	pw := &PreWarm{
		id:          uuid.New().String(),
		agent:       agent,
		cert:        cert,
		fingerprint: fingerprint,
		ufrag:       ufrag,
		pwd:         pwd,
		createdAt:   time.Now(),
		stunURIs:    stunURIs,
	}

	agent.OnConnectionStateChange(func(state ice.ConnectionState) {
		logf("[prewarm][%s] ICE connection state changed: %s", pw.id, state.String())
	})

	if err := agent.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			return
		}
		line := "a=candidate:" + c.Marshal()

		pw.mu.Lock()
		idx := len(pw.candidates)
		pw.candidates = append(pw.candidates, line)
		cb := pw.onLateCandidate
		readySent := pw.readySentCount > 0
		pw.mu.Unlock()

		logf("[prewarm][%s] gathered candidate: %s", pw.id, line)

		if readySent && idx >= pw.readySentCount && cb != nil {
			logf("[prewarm][%s] late candidate trickle: %s", pw.id, line)
			cb(line)
		}
	}); err != nil {
		agent.Close()
		return nil, fmt.Errorf("OnCandidate: %w", err)
	}

	if err := agent.GatherCandidates(); err != nil {
		agent.Close()
		return nil, fmt.Errorf("GatherCandidates: %w", err)
	}

	return pw, nil
}

func (pw *PreWarm) ToReadyPayload() *PreWarmReadyPayload {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	candsCopy := append([]string(nil), pw.candidates...)
	pw.readySentCount = len(candsCopy)

	return &PreWarmReadyPayload{
		PreWarmID:       pw.id,
		ICEUfrag:        pw.ufrag,
		ICEPwd:          pw.pwd,
		DTLSFingerprint: pw.fingerprint,
		Candidates:      candsCopy,
		GeneratedAt:     pw.createdAt.UnixMilli(),
	}
}

func (pw *PreWarm) BindToSession(s *rawShadowSession, bridgeID string) {
	pw.mu.Lock()
	pw.consumed = true
	pw.mu.Unlock()

	s.mu.Lock()
	s.cert = pw.cert
	s.iceAgent = pw.agent
	s.localCredentials = &RTCShadowReadyPayload{
		BridgeID:        bridgeID,
		ICEUfrag:        pw.ufrag,
		ICEPwd:          pw.pwd,
		DTLSFingerprint: "sha-256 " + pw.fingerprint,
		LocalIP:         "0.0.0.0",
		Candidates:      pw.candidates,
		GeneratedAt:     pw.createdAt.UnixMilli(),
		ExpiresAt:       time.Now().Add(60 * time.Second).UnixMilli(),
	}
	s.state = stateReady

	pw.mu.Lock()
	pw.onLateCandidate = func(line string) {
		s.mu.Lock()
		cb := s.onLateCandidate
		s.mu.Unlock()
		if cb != nil {
			cb(line)
		}
	}
	s.readySentCount = pw.readySentCount
	pw.mu.Unlock()

	s.mu.Unlock()
}

// AddICEServers updates the ICE agent configuration dynamically.
// If new ICE/TURN servers are provided, we recreate the agent using the same
// local credentials (ufrag/pwd) to preserve the munged SDP, but with new server URLs.
func (pw *PreWarm) AddICEServers(servers []IceServer, logf func(string, ...any)) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	stunURIs := iceServersToStunURIs(servers, logf, pw.id)
	if len(stunURIs) == 0 {
		return
	}

	logf("[prewarm][%s] Recreating agent to add %d ICE/TURN servers, keeping ufrag=%s", pw.id, len(stunURIs), pw.ufrag)

	// Close old agent first
	if pw.agent != nil {
		_ = pw.agent.Close()
	}

	agentConfig := &ice.AgentConfig{
		Urls: stunURIs,
		NetworkTypes: []ice.NetworkType{
			ice.NetworkTypeUDP4,
			ice.NetworkTypeTCP4,
		},
		LocalUfrag: pw.ufrag,
		LocalPwd:   pw.pwd,
	}

	agent, err := ice.NewAgent(agentConfig)
	if err != nil {
		logf("[prewarm][%s] Failed to recreate ICE agent with TURN: %v", pw.id, err)
		return
	}
	pw.agent = agent

	// Clear previous candidates and gather counts for the new agent run
	pw.candidates = nil
	pw.readySentCount = 0

	agent.OnConnectionStateChange(func(state ice.ConnectionState) {
		logf("[prewarm][%s] Recreated ICE connection state changed: %s", pw.id, state.String())
	})

	if err := agent.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			return
		}
		line := "a=candidate:" + c.Marshal()

		pw.mu.Lock()
		pw.candidates = append(pw.candidates, line)
		cb := pw.onLateCandidate
		pw.mu.Unlock()

		logf("[prewarm][%s] gathered recreated agent candidate: %s", pw.id, line)

		if cb != nil {
			logf("[prewarm][%s] late candidate trickle (recreated agent): %s", pw.id, line)
			cb(line)
		}
	}); err != nil {
		logf("[prewarm][%s] Failed to set OnCandidate on recreated agent: %v", pw.id, err)
		agent.Close()
		return
	}

	if err := agent.GatherCandidates(); err != nil {
		logf("[prewarm][%s] Failed to gather candidates on recreated agent: %v", pw.id, err)
		agent.Close()
		return
	}
}
