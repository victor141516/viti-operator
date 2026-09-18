package realtimebot

import (
	"sync"
	"testing"

	"github.com/pion/webrtc/v4/pkg/media"
)

type recordingSampleWriter struct {
	mu      sync.Mutex
	samples []media.Sample
}

func (writer *recordingSampleWriter) WriteSample(sample media.Sample) error {
	writer.mu.Lock()
	writer.samples = append(writer.samples, sample)
	writer.mu.Unlock()
	return nil
}

func TestPCMUPlayerFramesAndReportsPlayedAudioOnClear(t *testing.T) {
	writer := &recordingSampleWriter{}
	player := &pcmuPlayer{writer: writer, playedBytes: make(map[string]int)}
	if err := player.append("item-1", 0, make([]byte, 320)); err != nil {
		t.Fatal(err)
	}
	if err := player.writeFrame(); err != nil {
		t.Fatal(err)
	}
	point := player.clear()
	if point == nil || point.ItemID != "item-1" || point.ContentIndex != 0 || point.AudioEndMS != 20 {
		t.Fatalf("truncate point = %#v", point)
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.samples) != 1 || len(writer.samples[0].Data) != pcmuFrameBytes {
		t.Fatalf("samples = %#v", writer.samples)
	}
}

func TestPCMUPlayerBoundsQueuedAudio(t *testing.T) {
	player := &pcmuPlayer{writer: &recordingSampleWriter{}, playedBytes: make(map[string]int)}
	if err := player.append("item-1", 0, make([]byte, pcmuPlaybackLimit+1)); err == nil {
		t.Fatal("oversized playback queue was accepted")
	}
}
