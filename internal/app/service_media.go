package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

type serviceMediaCodec struct {
	parameters   webrtc.RTPCodecParameters
	asteriskName string
}

const serviceMediaSourceLimit = 1024

type serviceMediaSource struct {
	callID      string
	serviceName string
}

var serviceMediaCodecs = []serviceMediaCodec{
	{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1",
	}, PayloadType: 111}, "opus"},
	{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeG722, ClockRate: 8000,
	}, PayloadType: rtp.PayloadTypeG722}, "g722"},
	{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypePCMU, ClockRate: 8000,
	}, PayloadType: rtp.PayloadTypePCMU}, "ulaw"},
	{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypePCMA, ClockRate: 8000,
	}, PayloadType: rtp.PayloadTypePCMA}, "alaw"},
}

type serviceMediaManager struct {
	mu         sync.Mutex
	cfg        ServiceMediaConfig
	api        *webrtc.API
	iceMux     ice.UDPMux
	iceServers []webrtc.ICEServer
	bridge     serviceMediaBridge
	events     *EventHub
	logger     *slog.Logger
	hasHumans  func(Call) bool
	sessions   map[string]*serviceMediaSession
	byOwner    map[string]string
	sources    map[string]serviceMediaSource
	sourceIDs  []string
}

type serviceMediaSession struct {
	manager        *serviceMediaManager
	id             string
	call           Call
	serviceName    string
	connectionID   string
	recordingID    string
	purpose        string
	codec          serviceMediaCodec
	peer           *webrtc.PeerConnection
	asteriskRTP    *net.UDPConn
	localTrack     *webrtc.TrackLocalStaticRTP
	remoteMu       sync.RWMutex
	asteriskAddr   *net.UDPAddr
	asteriskPT     uint8
	closeOnce      sync.Once
	stateMu        sync.Mutex
	mediaConnected bool
	connectedOnce  sync.Once
	connected      chan struct{}
	terminated     chan struct{}
	bridgeMu       sync.Mutex
	bridged        bool
	closed         bool
	activated      bool
}

func newServiceMediaManager(cfg ServiceMediaConfig, configuredICE []ICEServerConfig, bridge serviceMediaBridge, events *EventHub, logger *slog.Logger, hasHumans ...func(Call) bool) (*serviceMediaManager, error) {
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
	for _, codec := range serviceMediaCodecs {
		if err := mediaEngine.RegisterCodec(codec.parameters, webrtc.RTPCodecTypeAudio); err != nil {
			_ = udpMux.Close()
			return nil, fmt.Errorf("register service media codec %s: %w", codec.parameters.MimeType, err)
		}
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
	manager := &serviceMediaManager{
		cfg: cfg, iceMux: udpMux, api: webrtc.NewAPI(
			webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(interceptors), webrtc.WithSettingEngine(settingEngine),
		),
		iceServers: iceServers, bridge: bridge, events: events, logger: logger,
		sessions: make(map[string]*serviceMediaSession), byOwner: make(map[string]string), sources: make(map[string]serviceMediaSource),
	}
	if len(hasHumans) != 0 {
		manager.hasHumans = hasHumans[0]
	}
	return manager, nil
}

func serviceMediaOwnerKey(serviceName, callID string) string {
	return serviceName + "\x00" + callID
}

func (m *serviceMediaManager) Attach(ctx context.Context, request serviceMediaAttachRequest) (serviceMediaDescription, error) {
	_, description, err := m.prepareSession(ctx, request, true)
	return description, err
}

func (m *serviceMediaManager) PrepareOutgoing(ctx context.Context, request serviceMediaAttachRequest) (serviceMediaOutgoingPreparation, error) {
	request.Purpose = "originator"
	session, description, err := m.prepareSession(ctx, request, false)
	if err != nil {
		return serviceMediaOutgoingPreparation{}, err
	}
	return serviceMediaOutgoingPreparation{
		Description: description,
		Activate: func(activateCtx context.Context, destination string) error {
			return session.activateOutgoing(activateCtx, destination)
		},
	}, nil
}

func (m *serviceMediaManager) prepareSession(ctx context.Context, request serviceMediaAttachRequest, joinExisting bool) (*serviceMediaSession, serviceMediaDescription, error) {
	if request.ServiceName == "" || request.ConnectionID == "" || request.Call.ID == "" {
		return nil, serviceMediaDescription{}, errors.New("service, connection, and call are required")
	}
	codec, err := selectServiceMediaCodec(request.OfferSDP)
	if err != nil {
		return nil, serviceMediaDescription{}, err
	}
	sessionID, err := randomID("media")
	if err != nil {
		return nil, serviceMediaDescription{}, errors.New("could not allocate media session")
	}
	recordingID, err := randomID("track")
	if err != nil {
		return nil, serviceMediaDescription{}, errors.New("could not allocate service recording segment")
	}
	rtpSocket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, serviceMediaDescription{}, fmt.Errorf("open Asterisk RTP bridge: %w", err)
	}
	peer, err := m.api.NewPeerConnection(webrtc.Configuration{ICEServers: m.iceServers})
	if err != nil {
		rtpSocket.Close()
		return nil, serviceMediaDescription{}, fmt.Errorf("create peer connection: %w", err)
	}
	localTrack, err := webrtc.NewTrackLocalStaticRTP(codec.parameters.RTPCodecCapability, "conference", "viti-"+request.Call.ID)
	if err != nil {
		peer.Close()
		rtpSocket.Close()
		return nil, serviceMediaDescription{}, fmt.Errorf("create outbound audio track: %w", err)
	}
	transceiver, err := peer.AddTransceiverFromTrack(localTrack, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	if err != nil {
		peer.Close()
		rtpSocket.Close()
		return nil, serviceMediaDescription{}, fmt.Errorf("add outbound audio track: %w", err)
	}
	if err := transceiver.SetCodecPreferences([]webrtc.RTPCodecParameters{codec.parameters}); err != nil {
		peer.Close()
		rtpSocket.Close()
		return nil, serviceMediaDescription{}, fmt.Errorf("select service media codec: %w", err)
	}
	sender := transceiver.Sender()
	session := &serviceMediaSession{
		manager: m, id: sessionID, call: request.Call, serviceName: request.ServiceName,
		connectionID: request.ConnectionID, recordingID: recordingID, purpose: request.Purpose, codec: codec, peer: peer,
		asteriskRTP: rtpSocket, localTrack: localTrack,
		connected: make(chan struct{}), terminated: make(chan struct{}),
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
		return nil, serviceMediaDescription{}, fmt.Errorf("apply SDP offer: %w", err)
	}
	answer, err := peer.CreateAnswer(nil)
	if err != nil {
		peer.Close()
		rtpSocket.Close()
		return nil, serviceMediaDescription{}, fmt.Errorf("create SDP answer: %w", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(answer); err != nil {
		peer.Close()
		rtpSocket.Close()
		return nil, serviceMediaDescription{}, fmt.Errorf("apply SDP answer: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		peer.Close()
		rtpSocket.Close()
		return nil, serviceMediaDescription{}, ctx.Err()
	}
	local := peer.LocalDescription()
	if local == nil {
		peer.Close()
		rtpSocket.Close()
		return nil, serviceMediaDescription{}, errors.New("SDP answer was not created")
	}
	rtpPort := rtpSocket.LocalAddr().(*net.UDPAddr).Port
	bridgeRequest := serviceMediaBridgeRequest{
		SessionID: sessionID, CallID: request.Call.ID, EngineCallID: request.Call.EngineCallID,
		TraceID: request.Call.TraceID, ServiceName: request.ServiceName, ConnectionID: request.ConnectionID,
		RecordingID: recordingID, RTPHost: m.cfg.AsteriskRTPHost, RTPPort: rtpPort, Codec: codec.asteriskName,
	}
	if joinExisting {
		if err := m.bridge.JoinServiceMedia(ctx, bridgeRequest); err != nil {
			peer.Close()
			rtpSocket.Close()
			return nil, serviceMediaDescription{}, fmt.Errorf("join Asterisk conference: %w", err)
		}
		session.bridged = true
	}

	ownerKey := serviceMediaOwnerKey(request.ServiceName, request.Call.ID)
	m.mu.Lock()
	var replaced *serviceMediaSession
	if previousID := m.byOwner[ownerKey]; previousID != "" {
		replaced = m.sessions[previousID]
	}
	m.sessions[sessionID] = session
	m.byOwner[ownerKey] = sessionID
	m.sources[sessionID] = serviceMediaSource{callID: request.Call.ID, serviceName: request.ServiceName}
	m.sourceIDs = append(m.sourceIDs, sessionID)
	m.pruneSourcesLocked()
	m.mu.Unlock()
	if replaced != nil {
		if replaced.purpose == "originator" && replaced.connectionID == session.connectionID {
			session.purpose = "originator"
		}
		replaced.close("replaced")
	}
	m.publishParticipant("joined", session, "attached")
	go session.forwardAsteriskAudio()
	session.peerStateChanged(peer.ConnectionState())
	return session, serviceMediaDescription{SessionID: sessionID, CallID: request.Call.ID, AnswerSDP: local.SDP, Purpose: session.purpose}, nil
}

func (s *serviceMediaSession) activateOutgoing(ctx context.Context, destination string) error {
	select {
	case <-s.connected:
	case <-s.terminated:
		return errors.New("media session ended before WebRTC became ready")
	case <-ctx.Done():
		return ctx.Err()
	}
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	if s.closed {
		return errors.New("media session is closed")
	}
	if s.activated {
		return errors.New("media session was already activated")
	}
	s.activated = true
	rtpPort := s.asteriskRTP.LocalAddr().(*net.UDPAddr).Port
	request := serviceMediaBridgeRequest{
		SessionID: s.id, CallID: s.call.ID, EngineCallID: s.call.EngineCallID, TraceID: s.call.TraceID,
		ServiceName: s.serviceName, ConnectionID: s.connectionID,
		RecordingID: s.recordingID, RTPHost: s.manager.cfg.AsteriskRTPHost, RTPPort: rtpPort, Codec: s.codec.asteriskName,
	}
	if err := s.manager.bridge.StartServiceCall(ctx, request, destination); err != nil {
		return fmt.Errorf("start service call through Asterisk: %w", err)
	}
	s.bridged = true
	return nil
}

func selectServiceMediaCodec(offer string) (serviceMediaCodec, error) {
	var description sdp.SessionDescription
	if err := description.UnmarshalString(offer); err != nil {
		return serviceMediaCodec{}, fmt.Errorf("parse SDP offer: %w", err)
	}
	sessionDirection := sdpMediaDirection(description.Attributes, "sendrecv")
	for _, media := range description.MediaDescriptions {
		if !strings.EqualFold(media.MediaName.Media, "audio") || media.MediaName.Port.Value == 0 {
			continue
		}
		if sdpMediaDirection(media.Attributes, sessionDirection) != "sendrecv" {
			continue
		}
		for _, rawPayloadType := range media.MediaName.Formats {
			value, err := strconv.ParseUint(rawPayloadType, 10, 8)
			if err != nil {
				continue
			}
			offered, err := description.GetCodecForPayloadType(uint8(value))
			if err != nil {
				continue
			}
			for _, supported := range serviceMediaCodecs {
				if strings.EqualFold(offered.Name, strings.TrimPrefix(supported.parameters.MimeType, "audio/")) {
					return supported, nil
				}
			}
		}
	}
	return serviceMediaCodec{}, errors.New("SDP offer does not include full-duplex audio using Opus, G.722, PCMU, or PCMA")
}

func sdpMediaDirection(attributes []sdp.Attribute, fallback string) string {
	for _, attribute := range attributes {
		switch strings.ToLower(attribute.Key) {
		case "sendrecv", "sendonly", "recvonly", "inactive":
			return strings.ToLower(attribute.Key)
		}
	}
	return fallback
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

func (m *serviceMediaManager) ResolveEngineSession(sessionID string) (string, string, bool) {
	m.mu.Lock()
	source, found := m.sources[sessionID]
	m.mu.Unlock()
	if !found {
		return "", "", false
	}
	return source.serviceName, source.callID, true
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
		payloadType := s.asteriskPT
		s.remoteMu.RUnlock()
		if destination == nil {
			continue
		}
		copyPacket := *packet
		copyPacket.PayloadType = payloadType
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
		var packet rtp.Packet
		if err := packet.Unmarshal(buffer[:count]); err != nil {
			continue
		}
		s.remoteMu.Lock()
		if s.asteriskAddr == nil {
			s.asteriskAddr = source
			s.asteriskPT = packet.PayloadType
		}
		allowed := s.asteriskAddr.IP.Equal(source.IP) && s.asteriskAddr.Port == source.Port
		s.remoteMu.Unlock()
		if !allowed {
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
		s.connectedOnce.Do(func() { close(s.connected) })
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
		s.manager.pruneSourcesLocked()
		s.manager.mu.Unlock()
		s.setMediaConnected(false, reason)
		s.bridgeMu.Lock()
		s.closed = true
		bridged := s.bridged
		s.bridgeMu.Unlock()
		close(s.terminated)
		_ = s.peer.Close()
		_ = s.asteriskRTP.Close()
		if bridged {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.manager.bridge.LeaveServiceMedia(ctx, s.id); err != nil {
				s.manager.logger.Warn("leave service media bridge", "session_id", s.id, "error", err)
			}
			cancel()
		}
		if bridged && s.purpose == "originator" && !s.manager.originatorOrHumanRemains(s.call) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := s.manager.bridge.EndServiceCall(ctx, s.call.EngineCallID); err != nil {
				s.manager.logger.Warn("end abandoned service call", "session_id", s.id, "call_id", s.call.ID, "error", err)
			}
			cancel()
		}
		s.manager.publishParticipant("left", s, reason)
	})
}

func (m *serviceMediaManager) pruneSourcesLocked() {
	if len(m.sources) <= serviceMediaSourceLimit {
		return
	}
	retained := m.sourceIDs[:0]
	for _, sessionID := range m.sourceIDs {
		if len(m.sources) > serviceMediaSourceLimit {
			if _, active := m.sessions[sessionID]; !active {
				delete(m.sources, sessionID)
				continue
			}
		}
		retained = append(retained, sessionID)
	}
	m.sourceIDs = retained
}

func (m *serviceMediaManager) originatorOrHumanRemains(call Call) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		if session.call.ID == call.ID && session.purpose == "originator" {
			return true
		}
	}
	return m.hasHumans != nil && m.hasHumans(call)
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
