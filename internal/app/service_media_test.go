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
}

func (b *fakeServiceMediaBridge) JoinServiceMedia(_ context.Context, request serviceMediaBridgeRequest) error {
	b.mu.Lock()
	b.joins = append(b.joins, request)
	b.mu.Unlock()
	return nil
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

	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if len(bridge.joins) != 2 || bridge.joins[0].RTPPort == 0 || bridge.joins[0].SessionID != first.SessionID {
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
	settings := webrtc.SettingEngine{}
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settings.SetIncludeLoopbackCandidate(true)
	peer, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
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
