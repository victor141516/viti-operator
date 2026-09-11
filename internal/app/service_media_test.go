package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

type fakeServiceMediaBridge struct {
	mu     sync.Mutex
	joins  []serviceMediaBridgeRequest
	leaves []string
	dtmf   []string
	starts []string
	ends   []string
}

func (b *fakeServiceMediaBridge) EndServiceCall(_ context.Context, engineCallID string) error {
	b.mu.Lock()
	b.ends = append(b.ends, engineCallID)
	b.mu.Unlock()
	return nil
}

func (b *fakeServiceMediaBridge) JoinServiceMedia(_ context.Context, request serviceMediaBridgeRequest) error {
	b.mu.Lock()
	b.joins = append(b.joins, request)
	b.mu.Unlock()
	return nil
}

func (b *fakeServiceMediaBridge) StartServiceCall(_ context.Context, request serviceMediaBridgeRequest, destination string) error {
	b.mu.Lock()
	b.joins = append(b.joins, request)
	b.starts = append(b.starts, destination)
	b.mu.Unlock()
	return nil
}

func TestServiceMediaOutgoingActivationWaitsForWebRTC(t *testing.T) {
	bridge := &fakeServiceMediaBridge{}
	manager, err := newServiceMediaManager(
		ServiceMediaConfig{Enabled: true, ListenAddress: "127.0.0.1:0", AdvertiseAddress: "127.0.0.1", AsteriskRTPHost: "127.0.0.1"},
		nil, bridge, NewEventHub(), slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.CloseCall("call-out", "test_complete")
		_ = manager.iceMux.Close()
	})
	call := Call{
		ID: "call-out", TraceID: "trace-out", EngineCallID: "call-out",
		Direction: DirectionOutgoing, State: "dialing", StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	client, offer := serviceMediaOffer(t)
	defer client.Close()
	preparation, err := manager.PrepareOutgoing(context.Background(), serviceMediaAttachRequest{
		ServiceName: "dialer", ConnectionID: "connection-out", Call: call, OfferSDP: offer,
	})
	if err != nil {
		t.Fatal(err)
	}
	bridge.mu.Lock()
	if len(bridge.joins) != 0 {
		t.Fatal("carrier dialing started before the service applied the SDP answer")
	}
	bridge.mu.Unlock()
	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: preparation.Description.AnswerSDP}); err != nil {
		t.Fatal(err)
	}
	activateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := preparation.Activate(activateCtx, "+12025550123"); err != nil {
		t.Fatal(err)
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if len(bridge.starts) != 1 || bridge.starts[0] != "+12025550123" || len(bridge.joins) != 1 {
		t.Fatalf("service call activation: starts=%#v joins=%#v", bridge.starts, bridge.joins)
	}
	bridge.mu.Unlock()
	manager.CloseConnection("connection-out", "control_disconnected")
	bridge.mu.Lock()
	if len(bridge.ends) != 1 || bridge.ends[0] != call.EngineCallID {
		t.Fatalf("abandoned service call ends = %#v", bridge.ends)
	}
}

func TestServiceMediaCodecSelectionFollowsFullDuplexOffer(t *testing.T) {
	offer := "v=0\r\n" +
		"o=- 1 1 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 0 111\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=sendrecv\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:111 opus/48000/2\r\n"
	codec, err := selectServiceMediaCodec(offer)
	if err != nil {
		t.Fatal(err)
	}
	if codec.asteriskName != "ulaw" || codec.parameters.MimeType != webrtc.MimeTypePCMU {
		t.Fatalf("selected codec = %#v", codec)
	}

	recvOnly := strings.Replace(offer, "a=sendrecv", "a=recvonly", 1)
	if _, err := selectServiceMediaCodec(recvOnly); err == nil {
		t.Fatal("receive-only offer was accepted as full duplex")
	}
}

func TestServiceMediaNegotiatesPCMUWithAsteriskBridge(t *testing.T) {
	bridge := &fakeServiceMediaBridge{}
	manager, err := newServiceMediaManager(
		ServiceMediaConfig{Enabled: true, ListenAddress: "127.0.0.1:0", AdvertiseAddress: "127.0.0.1", AsteriskRTPHost: "127.0.0.1"},
		nil, bridge, NewEventHub(), slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	call := Call{
		ID: "call-pcmu", TraceID: "trace-pcmu", EngineCallID: "engine-pcmu", Instance: "engine-1",
		Direction: DirectionIncoming, State: "active", StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	client, offer := serviceMediaOfferWithCodecs(t, []webrtc.RTPCodecParameters{{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000},
		PayloadType:        0,
	}})
	defer client.Close()
	description, err := manager.Attach(context.Background(), serviceMediaAttachRequest{
		ServiceName: "legacy-audio", ConnectionID: "connection-pcmu", Call: call, OfferSDP: offer, Purpose: "auxiliary",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.CloseCall(call.ID, "test_complete")
		_ = manager.iceMux.Close()
	})
	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: description.AnswerSDP}); err != nil {
		t.Fatal(err)
	}
	waitForPeerConnected(t, client, manager.sessions[description.SessionID].peer)
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if len(bridge.joins) != 1 || bridge.joins[0].Codec != "ulaw" {
		t.Fatalf("PCMU bridge request = %#v", bridge.joins)
	}
}

func TestServiceMediaOriginatorRemainsWhenHumanIsPresent(t *testing.T) {
	manager := &serviceMediaManager{
		sessions: make(map[string]*serviceMediaSession),
		hasHumans: func(call Call) bool {
			return call.ID == "call-human"
		},
	}
	if !manager.originatorOrHumanRemains(Call{ID: "call-human", EngineCallID: "engine-human"}) {
		t.Fatal("human participant did not retain the service-originated call")
	}
}

func (b *fakeServiceMediaBridge) LeaveServiceMedia(_ context.Context, sessionID string) error {
	b.mu.Lock()
	b.leaves = append(b.leaves, sessionID)
	b.mu.Unlock()
	return nil
}

func (b *fakeServiceMediaBridge) SendServiceDTMF(_ context.Context, sessionID, digit string, _ int) error {
	b.mu.Lock()
	b.dtmf = append(b.dtmf, sessionID+":"+digit)
	b.mu.Unlock()
	return nil
}

func TestServiceMediaBindsSessionToConnectionAndReplacesOwnerGeneration(t *testing.T) {
	bridge := &fakeServiceMediaBridge{}
	events := NewEventHub()
	manager, err := newServiceMediaManager(
		ServiceMediaConfig{Enabled: true, ListenAddress: "127.0.0.1:0", AdvertiseAddress: "127.0.0.1", AsteriskRTPHost: "127.0.0.1"},
		nil, bridge, events, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.CloseConnection("connection-2", "test_complete")
		_ = manager.iceMux.Close()
	})
	call := Call{
		ID: "call-1", TraceID: "trace-1", EngineCallID: "engine-call-1", Instance: "engine-1",
		Direction: DirectionIncoming, State: "active", StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	streamID, sequence := events.StreamState()
	_, eventStream, cancelEvents := events.Subscribe(streamID, sequence)
	defer cancelEvents()

	firstClient, firstOffer := serviceMediaOffer(t)
	defer firstClient.Close()
	first, err := manager.Attach(context.Background(), serviceMediaAttachRequest{
		ServiceName: "transcriber", ConnectionID: "connection-1", Call: call,
		OfferSDP: firstOffer, Purpose: "auxiliary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.AnswerSDP, "a=candidate:") {
		t.Fatalf("manager SDP answer omitted ICE candidates: %s", first.AnswerSDP)
	}
	if err := firstClient.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: first.AnswerSDP}); err != nil {
		t.Fatal(err)
	}
	waitForPeerConnected(t, firstClient, manager.sessions[first.SessionID].peer)
	waitForMediaEvent(t, eventStream, "call.participant.joined", first.SessionID)
	waitForMediaEvent(t, eventStream, "call.media.connected", first.SessionID)

	if err := manager.AddICECandidate(context.Background(), serviceMediaICERequest{
		ServiceName: "other-service", ConnectionID: "connection-1", SessionID: first.SessionID,
	}); err == nil {
		t.Fatal("a different service reused another service's media session")
	}
	if err := manager.SendDTMF(context.Background(), serviceMediaDTMFRequest{
		ServiceName: "transcriber", ConnectionID: "connection-1", CallID: call.ID,
		SessionID: first.SessionID, Digit: "5", DurationMS: 250,
	}); err != nil {
		t.Fatal(err)
	}

	secondClient, secondOffer := serviceMediaOffer(t)
	defer secondClient.Close()
	second, err := manager.Attach(context.Background(), serviceMediaAttachRequest{
		ServiceName: "transcriber", ConnectionID: "connection-2", Call: call,
		OfferSDP: secondOffer, Purpose: "auxiliary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := secondClient.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: second.AnswerSDP}); err != nil {
		t.Fatal(err)
	}
	waitForPeerConnected(t, secondClient, manager.sessions[second.SessionID].peer)
	waitForMediaEvent(t, eventStream, "call.participant.left", first.SessionID)
	waitForMediaEvent(t, eventStream, "call.participant.joined", second.SessionID)
	waitForMediaEvent(t, eventStream, "call.media.connected", second.SessionID)
	if _, err := manager.authorizedSession(first.SessionID, "transcriber", "connection-1"); err == nil {
		t.Fatal("replaced media generation remained addressable")
	}
	if serviceName, callID, found := manager.ResolveEngineSession(first.SessionID); !found || serviceName != "transcriber" || callID != call.ID {
		t.Fatalf("delayed DTMF source was lost after replacement: service=%q call=%q found=%t", serviceName, callID, found)
	}

	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if len(bridge.joins) != 2 || bridge.joins[0].RTPPort == 0 || bridge.joins[0].SessionID != first.SessionID ||
		bridge.joins[0].RecordingID == "" || bridge.joins[0].RecordingID == first.SessionID {
		t.Fatalf("bridge joins = %#v", bridge.joins)
	}
	if len(bridge.leaves) == 0 || bridge.leaves[0] != first.SessionID {
		t.Fatalf("bridge leaves = %#v", bridge.leaves)
	}
	if len(bridge.dtmf) != 1 || bridge.dtmf[0] != first.SessionID+":5" {
		t.Fatalf("bridge DTMF = %#v", bridge.dtmf)
	}
}

func waitForPeerConnected(t *testing.T, peers ...*webrtc.PeerConnection) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connected := true
		for _, peer := range peers {
			connected = connected && peer.ConnectionState() == webrtc.PeerConnectionStateConnected
		}
		if connected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	states := make([]webrtc.PeerConnectionState, len(peers))
	for index, peer := range peers {
		states[index] = peer.ConnectionState()
	}
	t.Fatalf("peer connections did not connect: %v", states)
}

func serviceMediaOffer(t *testing.T) (*webrtc.PeerConnection, string) {
	t.Helper()
	return serviceMediaOfferWithCodecs(t, nil)
}

func serviceMediaOfferWithCodecs(t *testing.T, codecs []webrtc.RTPCodecParameters) (*webrtc.PeerConnection, string) {
	t.Helper()
	settings := webrtc.SettingEngine{}
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settings.SetIncludeLoopbackCandidate(true)
	options := []func(*webrtc.API){webrtc.WithSettingEngine(settings)}
	if len(codecs) != 0 {
		mediaEngine := &webrtc.MediaEngine{}
		for _, codec := range codecs {
			if err := mediaEngine.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
				t.Fatal(err)
			}
		}
		options = append(options, webrtc.WithMediaEngine(mediaEngine))
	}
	peer, err := webrtc.NewAPI(options...).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	}); err != nil {
		peer.Close()
		t.Fatal(err)
	}
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		peer.Close()
		t.Fatal(err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(offer); err != nil {
		peer.Close()
		t.Fatal(err)
	}
	select {
	case <-gatherComplete:
	case <-time.After(5 * time.Second):
		peer.Close()
		t.Fatal("client ICE gathering timed out")
	}
	return peer, peer.LocalDescription().SDP
}

func waitForMediaEvent(t *testing.T, events <-chan Event, eventType, sessionID string) Event {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			payload, _ := event.Payload.(map[string]any)
			if event.Type == eventType && (payload["session_id"] == sessionID || payload["participant_id"] == sessionID) {
				return event
			}
		case <-deadline.C:
			t.Fatalf("did not receive %s for %s", eventType, sessionID)
		}
	}
}
