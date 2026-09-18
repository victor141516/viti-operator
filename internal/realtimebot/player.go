package realtimebot

import (
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	pcmuClockRate     = 8000
	pcmuFrameBytes    = 160
	pcmuPlaybackLimit = 10 * pcmuClockRate
)

type sampleWriter interface {
	WriteSample(media.Sample) error
}

type queuedAudio struct {
	itemID       string
	contentIndex int
	data         []byte
}

type truncatePoint struct {
	ItemID       string
	ContentIndex int
	AudioEndMS   int
}

type pcmuPlayer struct {
	writer sampleWriter

	mu          sync.Mutex
	queue       []queuedAudio
	queuedBytes int
	playedBytes map[string]int
	stop        chan struct{}
	done        chan struct{}
	errors      chan error
	closeOnce   sync.Once
}

func newPCMUPlayer(writer sampleWriter) *pcmuPlayer {
	player := &pcmuPlayer{
		writer: writer, playedBytes: make(map[string]int), stop: make(chan struct{}),
		done: make(chan struct{}), errors: make(chan error, 1),
	}
	go player.run()
	return player
}

func (player *pcmuPlayer) append(itemID string, contentIndex int, audio []byte) error {
	if len(audio) == 0 {
		return nil
	}
	player.mu.Lock()
	defer player.mu.Unlock()
	if player.queuedBytes+len(audio) > pcmuPlaybackLimit {
		return errors.New("OpenAI audio playback queue exceeded 10 seconds")
	}
	player.queue = append(player.queue, queuedAudio{
		itemID: itemID, contentIndex: contentIndex, data: append([]byte(nil), audio...),
	})
	player.queuedBytes += len(audio)
	return nil
}

func (player *pcmuPlayer) clear() *truncatePoint {
	player.mu.Lock()
	defer player.mu.Unlock()
	if len(player.queue) == 0 {
		return nil
	}
	current := player.queue[0]
	played := player.playedBytes[current.itemID]
	player.queue = nil
	player.queuedBytes = 0
	return &truncatePoint{
		ItemID: current.itemID, ContentIndex: current.contentIndex,
		AudioEndMS: played * 1000 / pcmuClockRate,
	}
}

func (player *pcmuPlayer) errorsChannel() <-chan error {
	return player.errors
}

func (player *pcmuPlayer) close() {
	player.closeOnce.Do(func() { close(player.stop) })
	<-player.done
}

func (player *pcmuPlayer) run() {
	defer close(player.done)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := player.writeFrame(); err != nil {
				select {
				case player.errors <- err:
				default:
				}
				return
			}
		case <-player.stop:
			return
		}
	}
}

func (player *pcmuPlayer) writeFrame() error {
	player.mu.Lock()
	if len(player.queue) == 0 {
		player.mu.Unlock()
		return nil
	}
	segment := &player.queue[0]
	count := min(len(segment.data), pcmuFrameBytes)
	frame := append([]byte(nil), segment.data[:count]...)
	itemID := segment.itemID
	segment.data = segment.data[count:]
	player.queuedBytes -= count
	if len(segment.data) == 0 {
		player.queue = player.queue[1:]
	}
	player.mu.Unlock()

	duration := time.Duration(count) * time.Second / pcmuClockRate
	if err := player.writer.WriteSample(media.Sample{Data: frame, Duration: duration}); err != nil {
		return err
	}
	player.mu.Lock()
	player.playedBytes[itemID] += count
	player.mu.Unlock()
	return nil
}
