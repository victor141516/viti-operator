package app

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const eventHistoryLimit = 256
const eventSubscriberBuffer = 64

type EventReplay struct {
	StreamID         string
	LatestSequence   uint64
	Events           []Event
	SnapshotRequired bool
}

type EventHub struct {
	mu          sync.Mutex
	streamID    string
	next        uint64
	history     []Event
	waiters     map[chan struct{}]struct{}
	subscribers map[chan Event]struct{}
	diag        *DiagnosticRecorder
}

func NewEventHub() *EventHub {
	streamID, err := randomID("stream")
	if err != nil {
		streamID = fmt.Sprintf("stream-%d", time.Now().UnixNano())
	}
	return &EventHub{
		streamID: streamID, next: 1,
		waiters: map[chan struct{}]struct{}{}, subscribers: map[chan Event]struct{}{},
	}
}

func (h *EventHub) SetDiagnostics(diagnostics *DiagnosticRecorder) {
	h.diag = diagnostics
}

func (h *EventHub) Publish(kind string, data any) Event {
	h.mu.Lock()
	now := time.Now().UTC()
	event := Event{
		Type: kind, Version: 1, EventID: fmt.Sprintf("%s-%d", h.streamID, h.next),
		StreamID: h.streamID, Sequence: h.next, OccurredAt: now, CallID: eventCallID(data),
		Payload: data, At: now, Data: data,
	}
	h.next++
	h.history = append(h.history, event)
	if len(h.history) > eventHistoryLimit {
		h.history = append([]Event(nil), h.history[len(h.history)-eventHistoryLimit:]...)
	}
	for waiter := range h.waiters {
		close(waiter)
		delete(h.waiters, waiter)
	}
	for subscriber := range h.subscribers {
		select {
		case subscriber <- event:
		default:
			close(subscriber)
			delete(h.subscribers, subscriber)
		}
	}
	h.mu.Unlock()
	traceID := ""
	if call, ok := data.(Call); ok {
		traceID = call.TraceID
	}
	h.diag.RecordTrace("manager.events", "published", traceID, event)
	return event
}

func eventCallID(data any) string {
	switch value := data.(type) {
	case Call:
		return value.ID
	case *Call:
		if value != nil {
			return value.ID
		}
	case map[string]any:
		if callID, ok := value["call_id"].(string); ok {
			return callID
		}
	}
	return ""
}

// Subscribe atomically captures replay state and installs a live subscriber so
// an integration cannot miss an event between replay and subscription.
func (h *EventHub) Subscribe(streamID string, after uint64) (EventReplay, <-chan Event, func()) {
	h.mu.Lock()
	latest := h.next - 1
	replay := EventReplay{StreamID: h.streamID, LatestSequence: latest}
	switch {
	case streamID == "":
		replay.SnapshotRequired = true
	case streamID != h.streamID:
		replay.SnapshotRequired = true
	case after > latest:
		replay.SnapshotRequired = true
	case len(h.history) > 0 && after+1 < h.history[0].Sequence:
		replay.SnapshotRequired = true
	default:
		replay.Events = h.afterLocked(after)
	}
	subscriber := make(chan Event, eventSubscriberBuffer)
	h.subscribers[subscriber] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		if _, found := h.subscribers[subscriber]; found {
			delete(h.subscribers, subscriber)
			close(subscriber)
		}
		h.mu.Unlock()
	}
	return replay, subscriber, cancel
}

func (h *EventHub) StreamState() (string, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streamID, h.next - 1
}

func (h *EventHub) Wait(ctx context.Context, after uint64) []Event {
	for {
		h.mu.Lock()
		if items := h.afterLocked(after); len(items) > 0 {
			h.mu.Unlock()
			h.diag.RecordEvent("manager.events", "wait.completed", map[string]any{"after": after, "count": len(items), "events": items})
			return items
		}
		waiter := make(chan struct{})
		h.waiters[waiter] = struct{}{}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			h.mu.Lock()
			delete(h.waiters, waiter)
			h.mu.Unlock()
			h.diag.RecordEvent("manager.events", "wait.timeout", map[string]any{"after": after, "error": errorString(ctx.Err())})
			return nil
		case <-waiter:
		}
	}
}

func (h *EventHub) afterLocked(after uint64) []Event {
	items := make([]Event, 0)
	for _, event := range h.history {
		if event.Sequence > after {
			items = append(items, event)
		}
	}
	return items
}
