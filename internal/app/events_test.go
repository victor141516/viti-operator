package app

import "testing"

func TestEventHubPublishesVersionedEnvelope(t *testing.T) {
	hub := NewEventHub()
	event := hub.Publish("call.updated", Call{ID: "call-one", TraceID: "trace-one"})
	if event.Type != "call.updated" || event.Version != 1 || event.Sequence != 1 {
		t.Fatalf("unexpected event envelope: %#v", event)
	}
	if event.EventID == "" || event.StreamID == "" || event.OccurredAt.IsZero() {
		t.Fatalf("missing event identity: %#v", event)
	}
	if event.CallID != "call-one" || event.At != event.OccurredAt || event.Data == nil || event.Payload == nil {
		t.Fatalf("missing event compatibility or call fields: %#v", event)
	}
}

func TestEventHubSubscriptionReplaysWithoutGap(t *testing.T) {
	hub := NewEventHub()
	first := hub.Publish("one", map[string]any{"value": 1})
	hub.Publish("two", map[string]any{"value": 2})
	replay, live, cancel := hub.Subscribe(first.StreamID, first.Sequence)
	defer cancel()
	if replay.SnapshotRequired || len(replay.Events) != 1 || replay.Events[0].Type != "two" {
		t.Fatalf("unexpected replay: %#v", replay)
	}
	hub.Publish("three", map[string]any{"value": 3})
	if event := <-live; event.Type != "three" {
		t.Fatalf("unexpected live event: %#v", event)
	}
}

func TestEventHubSubscriptionRequiresSnapshotForUnknownGenerationOrGap(t *testing.T) {
	hub := NewEventHub()
	streamID, _ := hub.StreamState()
	replay, _, cancel := hub.Subscribe("another-stream", 0)
	cancel()
	if !replay.SnapshotRequired {
		t.Fatal("expected a snapshot for another process generation")
	}
	for index := 0; index < eventHistoryLimit+1; index++ {
		hub.Publish("event", index)
	}
	replay, _, cancel = hub.Subscribe(streamID, 0)
	cancel()
	if !replay.SnapshotRequired {
		t.Fatal("expected a snapshot for a cursor older than the in-memory buffer")
	}
}
