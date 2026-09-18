package realtimebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type mediaDescription struct {
	SessionID string `json:"session_id"`
	CallID    string `json:"call_id"`
	AnswerSDP string `json:"sdp_answer"`
}

type mediaSession struct {
	peer      *webrtc.PeerConnection
	player    *pcmuPlayer
	connected chan struct{}
	errors    chan error
	closeOnce sync.Once
}

func attachMedia(ctx context.Context, client *vitiClient, callID, purpose string, onAudio func([]byte) error) (*mediaSession, error) {
	mediaEngine := &webrtc.MediaEngine{}
	codec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: pcmuClockRate},
		PayloadType:        rtp.PayloadTypePCMU,
	}
	if err := mediaEngine.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register PCMU codec: %w", err)
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptors); err != nil {
		return nil, fmt.Errorf("register WebRTC interceptors: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(interceptors))
	peer, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("create Viti peer connection: %w", err)
	}
	localTrack, err := webrtc.NewTrackLocalStaticSample(codec.RTPCodecCapability, "openai", "viti-"+callID)
	if err != nil {
		_ = peer.Close()
		return nil, fmt.Errorf("create Viti output track: %w", err)
	}
	transceiver, err := peer.AddTransceiverFromTrack(localTrack, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	if err != nil {
		_ = peer.Close()
		return nil, fmt.Errorf("add Viti audio transceiver: %w", err)
	}
	if err := transceiver.SetCodecPreferences([]webrtc.RTPCodecParameters{codec}); err != nil {
		_ = peer.Close()
		return nil, fmt.Errorf("select PCMU: %w", err)
	}
	session := &mediaSession{
		peer: peer, player: newPCMUPlayer(localTrack), connected: make(chan struct{}), errors: make(chan error, 1),
	}
	go func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, err := transceiver.Sender().Read(buffer); err != nil {
				return
			}
		}
	}()
	peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						session.report(fmt.Errorf("read Viti RTP: %w", err))
					}
					return
				}
				if err := onAudio(packet.Payload); err != nil {
					session.report(err)
					return
				}
			}
		}()
	})
	var connectedOnce sync.Once
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connectedOnce.Do(func() { close(session.connected) })
		case webrtc.PeerConnectionStateFailed:
			session.report(errors.New("Viti WebRTC connection failed"))
		case webrtc.PeerConnectionStateClosed:
			session.report(errors.New("Viti WebRTC connection closed"))
		}
	})
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		session.close()
		return nil, fmt.Errorf("create Viti SDP offer: %w", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(offer); err != nil {
		session.close()
		return nil, fmt.Errorf("apply Viti SDP offer: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		session.close()
		return nil, ctx.Err()
	}
	local := peer.LocalDescription()
	if local == nil {
		session.close()
		return nil, errors.New("Viti SDP offer was not created")
	}
	payload, err := client.command(ctx, "media.attach", callID, map[string]any{
		"sdp_offer": local.SDP, "purpose": purpose,
	})
	if err != nil {
		session.close()
		return nil, err
	}
	var description mediaDescription
	if err := json.Unmarshal(payload, &description); err != nil || description.AnswerSDP == "" {
		session.close()
		return nil, errors.New("Viti returned an invalid media description")
	}
	if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: description.AnswerSDP}); err != nil {
		session.close()
		return nil, fmt.Errorf("apply Viti SDP answer: %w", err)
	}
	connectTimer := time.NewTimer(15 * time.Second)
	defer connectTimer.Stop()
	select {
	case <-session.connected:
		return session, nil
	case err := <-session.errors:
		session.close()
		return nil, err
	case <-connectTimer.C:
		session.close()
		return nil, errors.New("Viti WebRTC connection timed out")
	case <-ctx.Done():
		session.close()
		return nil, ctx.Err()
	}
}

func (session *mediaSession) report(err error) {
	select {
	case session.errors <- err:
	default:
	}
}

func (session *mediaSession) wait(ctx context.Context) error {
	select {
	case err := <-session.errors:
		return err
	case err := <-session.player.errorsChannel():
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (session *mediaSession) playPCMU(ctx context.Context, audio []byte) error {
	for offset := 0; offset < len(audio); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		count := min(pcmuFrameBytes, len(audio)-offset)
		frame := append([]byte(nil), audio[offset:offset+count]...)
		duration := time.Duration(count) * time.Second / pcmuClockRate
		if err := session.player.writer.WriteSample(media.Sample{Data: frame, Duration: duration}); err != nil {
			return fmt.Errorf("play greeting audio: %w", err)
		}
		offset += count
		timer := time.NewTimer(duration)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return nil
}

func (session *mediaSession) close() {
	session.closeOnce.Do(func() {
		session.player.close()
		_ = session.peer.Close()
	})
}
