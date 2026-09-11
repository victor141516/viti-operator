package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const asteriskOpusPayloadType = 107

type serviceMediaManager struct {
	mu         sync.Mutex
	cfg        ServiceMediaConfig
	api        *webrtc.API
	iceMux     ice.UDPMux
	iceServers []webrtc.ICEServer
	bridge     serviceMediaBridge
	events     *EventHub
	logger     *slog.Logger
	sessions   map[string]*serviceMediaSession
	byOwner    map[string]string
}

type serviceMediaSession struct {
	manager        *serviceMediaManager
	id             string
	call           Call
	serviceName    string
	connectionID   string
	purpose        string
	peer           *webrtc.PeerConnection
	asteriskRTP    *net.UDPConn
	localTrack     *webrtc.TrackLocalStaticRTP
	remoteMu       sync.RWMutex
	asteriskAddr   *net.UDPAddr
	closeOnce      sync.Once
	stateMu        sync.Mutex
	mediaConnected bool
}

func newServiceMediaManager(cfg ServiceMediaConfig, configuredICE []ICEServerConfig, bridge serviceMediaBridge, events *EventHub, logger *slog.Logger) (*serviceMediaManager, error) {
	listenAddress, err := net.ResolveUDPAddr("udp", cfg.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("resolve ICE listen address: %w", err)
	}
	iceSocket, err := net.ListenUDP("udp", listenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen for service ICE: %w", err)
	}
	udpMux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: iceSocket})
	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetICEUDPMux(udpMux)
	settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	if listenAddress.IP.IsLoopback() {
		settingEngine.SetIncludeLoopbackCandidate(true)
	}
	if advertised := strings.TrimSpace(cfg.AdvertiseAddress); advertised != "" {
		if parsed := net.ParseIP(advertised); parsed == nil {
			_ = udpMux.Close()
			return nil, errors.New("service media advertise_address must be an IP address")
		}
		settingEngine.SetNAT1To1IPs([]string{advertised}, webrtc.ICECandidateTypeHost)
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		_ = udpMux.Close()
		return nil, fmt.Errorf("register Opus codec: %w", err)
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptors); err != nil {
		_ = udpMux.Close()
		return nil, fmt.Errorf("register WebRTC interceptors: %w", err)
	}
	iceServers := make([]webrtc.ICEServer, 0, len(configuredICE))
	for _, server := range configuredICE {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs: append([]string(nil), server.URLs...), Username: server.Username, Credential: server.Credential,
		})
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &serviceMediaManager{
		cfg: cfg, iceMux: udpMux, api: webrtc.NewAPI(
			webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(interceptors), webrtc.WithSettingEngine(settingEngine),
		),
		iceServers: iceServers, bridge: bridge, events: events, logger: logger,
		sessions: make(map[string]*serviceMediaSession), byOwner: make(map[string]string),
	}, nil
}

func serviceMediaOwnerKey(serviceName, callID string) string {
	return serviceName + "\x00" + callID
}

func (m *serviceMediaManager) Attach(ctx context.Context, request serviceMediaAttachRequest) (serviceMediaDescription, error) {
	if request.ServiceName == "" || request.ConnectionID == "" || request.Call.ID == "" {
		return serviceMediaDescription{}, errors.New("service, connection, and call are required")
	}
	sessionID, err := randomID("media")
	if err != nil {
		return serviceMediaDescription{}, errors.New("could not allocate media session")
	}
	rtpSocket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return serviceMediaDescription{}, fmt.Errorf("open Asterisk RTP bridge: %w", err)
	}
	peer, err := m.api.NewPeerConnection(webrtc.Configuration{ICEServers: m.iceServers})
	if err != nil {
		rtpSocket.Close()
		return serviceMediaDescription{}, fmt.Errorf("create peer connection: %w", err)
	}
	localTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	}, "conference", "viti-"+request.Call.ID)
	if err != nil {
		peer.Close()
		rtpSocket.Close()
		return serviceMediaDescription{}, fmt.Errorf("create outbound audio track: %w", err)
	}
	sender, err := peer.AddTrack(localTrack)
	if err != nil {
		peer.Close()
		rtpSocket.Close()
		return serviceMediaDescription{}, fmt.Errorf("add outbound audio track: %w", err)
	}
	session := &serviceMediaSession{
		manager: m, id: sessionID, call: request.Call, serviceName: request.ServiceName,
		connectionID: request.ConnectionID, purpose: request.Purpose, peer: peer,
		asteriskRTP: rtpSocket, localTrack: localTrack,
	}
	go func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buffer); err != nil {
				return
			}
		}
	}()
	peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go session.forwardServiceAudio(track)
	})
	peer.OnConnectionStateChange(session.peerStateChanged)
	if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: request.OfferSDP}); err != nil {
		peer.Close()
		rtpSocket.Close()
		return serviceMediaDescription{}, fmt.Errorf("apply SDP offer: %w", err)
	}
	answer, err := peer.CreateAnswer(nil)
	if err != nil {
		peer.Close()
		rtpSocket.Close()
		return serviceMediaDescription{}, fmt.Errorf("create SDP answer: %w", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(answer); err != nil {
		peer.Close()
		rtpSocket.Close()
		return serviceMediaDescription{}, fmt.Errorf("apply SDP answer: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		peer.Close()
		rtpSocket.Close()
		return serviceMediaDescription{}, ctx.Err()
	}
	local := peer.LocalDescription()
	if local == nil {
		peer.Close()
		rtpSocket.Close()
		return serviceMediaDescription{}, errors.New("SDP answer was not created")
	}
	rtpPort := rtpSocket.LocalAddr().(*net.UDPAddr).Port
	bridgeRequest := serviceMediaBridgeRequest{
		SessionID: sessionID, CallID: request.Call.ID, EngineCallID: request.Call.EngineCallID,
		TraceID: request.Call.TraceID, ServiceName: request.ServiceName, ConnectionID: request.ConnectionID,
		RTPHost: m.cfg.AsteriskRTPHost, RTPPort: rtpPort,
	}
	if err := m.bridge.JoinServiceMedia(ctx, bridgeRequest); err != nil {
		peer.Close()
		rtpSocket.Close()
		return serviceMediaDescription{}, fmt.Errorf("join Asterisk conference: %w", err)
	}

	ownerKey := serviceMediaOwnerKey(request.ServiceName, request.Call.ID)
	m.mu.Lock()
	var replaced *serviceMediaSession
	if previousID := m.byOwner[ownerKey]; previousID != "" {
		replaced = m.sessions[previousID]
	}
	m.sessions[sessionID] = session
	m.byOwner[ownerKey] = sessionID
	m.mu.Unlock()
	if replaced != nil {
		replaced.close("replaced")
	}
	m.publishParticipant("joined", session, "attached")
	go session.forwardAsteriskAudio()
	return serviceMediaDescription{SessionID: sessionID, CallID: request.Call.ID, AnswerSDP: local.SDP, Purpose: request.Purpose}, nil
}

func (m *serviceMediaManager) AddICECandidate(_ context.Context, request serviceMediaICERequest) error {
	session, err := m.authorizedSession(request.SessionID, request.ServiceName, request.ConnectionID)
	if err != nil {
		return err
	}
	return session.peer.AddICECandidate(webrtc.ICECandidateInit{
		Candidate: request.Candidate, SDPMid: request.SDPMid, SDPMLineIndex: request.SDPMLine,
	})
}

func (m *serviceMediaManager) SendDTMF(ctx context.Context, request serviceMediaDTMFRequest) error {
	m.mu.Lock()
	sessionID := request.SessionID
	if sessionID == "" {
		sessionID = m.byOwner[serviceMediaOwnerKey(request.ServiceName, request.CallID)]
	}
	m.mu.Unlock()
	session, err := m.authorizedSession(sessionID, request.ServiceName, request.ConnectionID)
	if err != nil {
		return err
	}
	if session.call.ID != request.CallID {
		return errors.New("media session belongs to a different call")
	}
	return m.bridge.SendServiceDTMF(ctx, session.id, request.Digit, request.DurationMS)
}

func (m *serviceMediaManager) authorizedSession(sessionID, serviceName, connectionID string) (*serviceMediaSession, error) {
	m.mu.Lock()
	session := m.sessions[sessionID]
	m.mu.Unlock()
	if session == nil {
		return nil, errors.New("media session not found")
	}
	if session.serviceName != serviceName || session.connectionID != connectionID {
		return nil, errors.New("media session belongs to a different service connection")
	}
	return session, nil
}

func (m *serviceMediaManager) CloseConnection(connectionID, reason string) {
	m.closeMatching(func(session *serviceMediaSession) bool { return session.connectionID == connectionID }, reason)
}

func (m *serviceMediaManager) CloseCall(callID, reason string) {
	m.closeMatching(func(session *serviceMediaSession) bool { return session.call.ID == callID }, reason)
}

func (m *serviceMediaManager) closeMatching(match func(*serviceMediaSession) bool, reason string) {
	m.mu.Lock()
	sessions := make([]*serviceMediaSession, 0)
	for _, session := range m.sessions {
		if match(session) {
			sessions = append(sessions, session)
		}
	}
	m.mu.Unlock()
	for _, session := range sessions {
		session.close(reason)
	}
}

func (s *serviceMediaSession) forwardServiceAudio(track *webrtc.TrackRemote) {
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		s.remoteMu.RLock()
		destination := s.asteriskAddr
		s.remoteMu.RUnlock()
		if destination == nil {
			continue
		}
		copyPacket := *packet
		copyPacket.PayloadType = asteriskOpusPayloadType
		encoded, err := copyPacket.Marshal()
		if err == nil {
			_, _ = s.asteriskRTP.WriteToUDP(encoded, destination)
		}
	}
}

func (s *serviceMediaSession) forwardAsteriskAudio() {
	buffer := make([]byte, 2048)
	for {
		count, source, err := s.asteriskRTP.ReadFromUDP(buffer)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.manager.logger.Warn("read service RTP bridge", "session_id", s.id, "error", err)
			}
			return
		}
		s.remoteMu.Lock()
		if s.asteriskAddr == nil {
			s.asteriskAddr = source
		}
		allowed := s.asteriskAddr.IP.Equal(source.IP) && s.asteriskAddr.Port == source.Port
		s.remoteMu.Unlock()
		if !allowed {
			continue
		}
		var packet rtp.Packet
		if err := packet.Unmarshal(buffer[:count]); err != nil {
			continue
		}
		if err := s.localTrack.WriteRTP(&packet); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			s.manager.logger.Warn("write service WebRTC track", "session_id", s.id, "error", err)
		}
	}
}

func (s *serviceMediaSession) peerStateChanged(state webrtc.PeerConnectionState) {
	s.manager.mu.Lock()
	registered := s.manager.sessions[s.id] == s
	s.manager.mu.Unlock()
	if !registered {
		return
	}
	switch state {
	case webrtc.PeerConnectionStateConnected:
		s.setMediaConnected(true, "ice_connected")
	case webrtc.PeerConnectionStateDisconnected:
		s.setMediaConnected(false, "ice_disconnected")
	case webrtc.PeerConnectionStateFailed:
		s.close("ice_failed")
	case webrtc.PeerConnectionStateClosed:
		s.close("peer_closed")
	}
}

func (s *serviceMediaSession) setMediaConnected(connected bool, reason string) {
	s.stateMu.Lock()
	if s.mediaConnected == connected {
		s.stateMu.Unlock()
		return
	}
	s.mediaConnected = connected
	s.stateMu.Unlock()
	state := "disconnected"
	if connected {
		state = "connected"
	}
	s.manager.publishMedia(state, s, reason)
}

func (s *serviceMediaSession) close(reason string) {
	s.closeOnce.Do(func() {
		s.manager.mu.Lock()
		delete(s.manager.sessions, s.id)
		ownerKey := serviceMediaOwnerKey(s.serviceName, s.call.ID)
		if s.manager.byOwner[ownerKey] == s.id {
			delete(s.manager.byOwner, ownerKey)
		}
		s.manager.mu.Unlock()
		s.setMediaConnected(false, reason)
		_ = s.peer.Close()
		_ = s.asteriskRTP.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.manager.bridge.LeaveServiceMedia(ctx, s.id); err != nil {
			s.manager.logger.Warn("leave service media bridge", "session_id", s.id, "error", err)
		}
		cancel()
		s.manager.publishParticipant("left", s, reason)
	})
}

func (m *serviceMediaManager) publishParticipant(state string, session *serviceMediaSession, reason string) {
	m.events.Publish("call.participant."+state, map[string]any{
		"call_id": session.call.ID, "trace_id": session.call.TraceID, "participant_id": session.id,
		"service_name": session.serviceName, "connection_id": session.connectionID,
		"role": "service", "purpose": session.purpose, "reason": reason,
	})
}

func (m *serviceMediaManager) publishMedia(state string, session *serviceMediaSession, reason string) {
	m.events.Publish("call.media."+state, map[string]any{
		"call_id": session.call.ID, "trace_id": session.call.TraceID, "participant_id": session.id,
		"session_id": session.id, "service_name": session.serviceName, "connection_id": session.connectionID,
		"role": "service", "purpose": session.purpose, "reason": reason,
	})
}
