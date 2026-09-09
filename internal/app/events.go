package app

import (
	"context"
	"sync"
	"time"
)

const eventHistoryLimit = 256

type EventHub struct {
	mu      sync.Mutex
	next    uint64
	history []Event
	waiters map[chan struct{}]struct{}
	diag    *DiagnosticRecorder
}

func NewEventHub() *EventHub {
	return &EventHub{next: 1, waiters: map[chan struct{}]struct{}{}}
}

func (h *EventHub) SetDiagnostics(diagnostics *DiagnosticRecorder) {
	h.diag = diagnostics
}

func (h *EventHub) Publish(kind string, data any) Event {
	h.mu.Lock()
	event := Event{Sequence: h.next, Type: kind, At: time.Now().UTC(), Data: data}
	h.next++
	h.history = append(h.history, event)
	if len(h.history) > eventHistoryLimit {
		h.history = append([]Event(nil), h.history[len(h.history)-eventHistoryLimit:]...)
	}
	for waiter := range h.waiters {
		close(waiter)
		delete(h.waiters, waiter)
	}
	h.mu.Unlock()
	traceID := ""
	if call, ok := data.(Call); ok {
		traceID = call.TraceID
	}
	h.diag.RecordTrace("manager.events", "published", traceID, event)
	return event
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
