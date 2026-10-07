package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const historyChangeLimit = 256
const historyChangeBytes = 4 << 20

// The journal is volatile. A new generation intentionally invalidates old
// checkpoints, while the authoritative history remains in the existing store.
type historyJournal struct {
	generation      string
	revision        uint64
	messages        map[string]json.RawMessage
	calls           map[string]json.RawMessage
	contactsVersion uint64
	changes         []historyChange
	bytes           int
	waiters         map[chan struct{}]struct{}
}

type historyChange struct {
	revision        uint64
	messages        map[string]json.RawMessage
	calls           map[string]json.RawMessage
	deletedMessages []string
	deletedCalls    []string
	bytes           int
}

type historySync struct {
	Type            string            `json:"type"`
	Version         int               `json:"version"`
	Mode            string            `json:"mode"`
	Reason          string            `json:"reason,omitempty"`
	BaseCursor      string            `json:"base_cursor"`
	Cursor          string            `json:"cursor"`
	Messages        []json.RawMessage `json:"messages"`
	Calls           []json.RawMessage `json:"calls"`
	DeletedMessages []string          `json:"deleted_messages"`
	DeletedCalls    []string          `json:"deleted_calls"`
	ContactsVersion uint64            `json:"contacts_version"`
}

func (s *Store) historyRowsLocked() (map[string]json.RawMessage, map[string]json.RawMessage) {
	messages, calls := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	for id, original := range s.messages {
		item := cloneMessage(original)
		peer := item.From
		if item.Direction == DirectionOutgoing {
			peer = item.To
		}
		item.ContactID, item.ContactName = "", ""
		if contact, found := resolveContact(s.contactView, peer); found {
			item.ContactID, item.ContactName = contact.ID, contact.Name
		}
		messages[id], _ = json.Marshal(item)
	}
	for id, item := range s.calls {
		item.ContactID, item.ContactName = "", ""
		if contact, found := resolveContact(s.contactView, item.Peer); found {
			item.ContactID, item.ContactName = contact.ID, contact.Name
		}
		calls[id], _ = json.Marshal(callListView(item))
	}
	return messages, calls
}

func (s *Store) ensureHistoryLocked() {
	if s.history != nil {
		return
	}
	generation, err := randomID("history")
	if err != nil {
		generation = "history-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	messages, calls := s.historyRowsLocked()
	s.history = &historyJournal{generation: generation, messages: messages, calls: calls,
		contactsVersion: s.contactsVersion, waiters: map[chan struct{}]struct{}{}}
}

func diffHistoryRows(before, after map[string]json.RawMessage) (map[string]json.RawMessage, []string, int) {
	updated, deleted, size := map[string]json.RawMessage{}, []string{}, 0
	for id, data := range after {
		if !bytes.Equal(before[id], data) {
			updated[id] = data
			size += len(id) + len(data)
		}
	}
	for id := range before {
		if _, found := after[id]; !found {
			deleted = append(deleted, id)
			size += len(id)
		}
	}
	sort.Strings(deleted)
	return updated, deleted, size
}

// Called only after a successful durable write, while holding the store lock.
// Comparing encoded public rows covers all mutation paths, including older SMS
// receipts, screening results, recording tracks and contact-name changes.
func (s *Store) recordHistoryLocked() {
	h := s.history
	messages, calls := s.historyRowsLocked()
	updatedMessages, deletedMessages, messageBytes := diffHistoryRows(h.messages, messages)
	updatedCalls, deletedCalls, callBytes := diffHistoryRows(h.calls, calls)
	if messageBytes+callBytes == 0 && h.contactsVersion == s.contactsVersion {
		return
	}
	h.revision++
	change := historyChange{revision: h.revision, messages: updatedMessages, calls: updatedCalls,
		deletedMessages: deletedMessages, deletedCalls: deletedCalls, bytes: messageBytes + callBytes}
	h.changes = append(h.changes, change)
	h.bytes += change.bytes
	for len(h.changes) > historyChangeLimit || h.bytes > historyChangeBytes {
		h.bytes -= h.changes[0].bytes
		h.changes = h.changes[1:]
	}
	h.messages, h.calls, h.contactsVersion = messages, calls, s.contactsVersion
	for waiter := range h.waiters {
		select {
		case waiter <- struct{}{}:
		default:
		}
	}
}

func historyValues(rows map[string]json.RawMessage) []json.RawMessage {
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		result = append(result, rows[id])
	}
	return result
}

func (s *Store) syncHistory(cursor string) historySync {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureHistoryLocked()
	h := s.history
	current := h.generation + ":" + strconv.FormatUint(h.revision, 10)
	result := historySync{Type: "history.sync", Version: 1, Mode: "delta", BaseCursor: cursor, Cursor: current,
		Messages: []json.RawMessage{}, Calls: []json.RawMessage{}, DeletedMessages: []string{}, DeletedCalls: []string{},
		ContactsVersion: h.contactsVersion}
	parts := strings.Split(cursor, ":")
	var after uint64
	valid := len(parts) == 2 && parts[0] == h.generation
	if valid {
		var err error
		after, err = strconv.ParseUint(parts[1], 10, 64)
		valid = err == nil && parts[1] == strconv.FormatUint(after, 10) && after <= h.revision
	}
	if valid && after < h.revision {
		valid = len(h.changes) > 0 && after >= h.changes[0].revision-1
	}
	if !valid {
		result.Mode = "full"
		if cursor != "" {
			result.Reason = "cursor_invalid"
		}
		result.Messages, result.Calls = historyValues(h.messages), historyValues(h.calls)
		return result
	}
	messages, calls := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	deletedMessages, deletedCalls := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	for _, change := range h.changes {
		if change.revision <= after {
			continue
		}
		for id, data := range change.messages {
			messages[id] = data
			delete(deletedMessages, id)
		}
		for id, data := range change.calls {
			calls[id] = data
			delete(deletedCalls, id)
		}
		for _, id := range change.deletedMessages {
			delete(messages, id)
			deletedMessages[id] = nil
		}
		for _, id := range change.deletedCalls {
			delete(calls, id)
			deletedCalls[id] = nil
		}
	}
	result.Messages, result.Calls = historyValues(messages), historyValues(calls)
	for id := range deletedMessages {
		result.DeletedMessages = append(result.DeletedMessages, id)
	}
	for id := range deletedCalls {
		result.DeletedCalls = append(result.DeletedCalls, id)
	}
	sort.Strings(result.DeletedMessages)
	sort.Strings(result.DeletedCalls)
	return result
}

func (s *Store) subscribeHistory() (<-chan struct{}, func()) {
	s.mu.Lock()
	s.ensureHistoryLocked()
	waiter := make(chan struct{}, 1)
	s.history.waiters[waiter] = struct{}{}
	s.mu.Unlock()
	return waiter, func() { s.mu.Lock(); delete(s.history.waiters, waiter); s.mu.Unlock() }
}

func (a *App) historySnapshot(w http.ResponseWriter, r *http.Request) {
	a.callsWithRecordingTiming()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, a.store.syncHistory(r.URL.Query().Get("cursor")))
}

func (a *App) activeCallState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	call, found := a.store.GetCallByTraceID(strings.TrimSpace(r.URL.Query().Get("trace_id")))
	writeJSON(w, http.StatusOK, map[string]bool{"active": found && call.TraceID != "" && call.EndedAt == nil && call.State == "active"})
}

// This is an application-data socket, not a bot or SIP/media connection.
func (a *App) historyWebSocket(w http.ResponseWriter, r *http.Request) {
	socket, err := integrationUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer socket.Close()
	a.callsWithRecordingTiming()
	socket.SetReadLimit(4096)
	_ = socket.SetReadDeadline(time.Now().Add(75 * time.Second))
	socket.SetPongHandler(func(string) error { return socket.SetReadDeadline(time.Now().Add(75 * time.Second)) })
	wake, unsubscribe := a.store.subscribeHistory()
	defer unsubscribe()
	_, events, cancelEvents := a.events.Subscribe("", 0)
	defer cancelEvents()
	requests := make(chan string, 1)
	done := make(chan struct{})
	writerDone := make(chan struct{})
	defer close(writerDone)
	go func() {
		defer close(done)
		for {
			var request struct {
				Type   string `json:"type"`
				Cursor string `json:"cursor"`
			}
			if socket.ReadJSON(&request) != nil || request.Type != "sync" || len(request.Cursor) > 128 {
				return
			}
			select {
			case requests <- request.Cursor:
			case <-writerDone:
				return
			case <-r.Context().Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	cursor := r.URL.Query().Get("cursor")
	writeSync := func(requestCursor string) bool {
		update := a.store.syncHistory(requestCursor)
		_ = socket.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if socket.WriteJSON(update) != nil {
			return false
		}
		cursor = update.Cursor
		return true
	}
	if !writeSync(cursor) {
		return
	}
	for {
		select {
		case <-done:
			return
		case <-r.Context().Done():
			return
		case request := <-requests:
			if !writeSync(request) {
				return
			}
		case <-wake:
			update := a.store.syncHistory(cursor)
			if update.Cursor == cursor {
				continue
			}
			if !writeSync(cursor) {
				return
			}
		case event, open := <-events:
			if !open {
				return
			}
			if event.Type == "call.updated" && event.CallID != "" {
				if call, found := a.store.GetCall(event.CallID); found && call.EndedAt != nil {
					for _, track := range call.RecordingTracks {
						timed := a.recordingTrackTiming(call, track)
						if timed.OffsetMS != track.OffsetMS || timed.DurationMS != track.DurationMS {
							_, _, _ = a.store.UpdateRecordingTrackTiming(call.ID, track.ID, timed.OffsetMS, timed.DurationMS)
						}
					}
				}
				continue
			}
			// Never forward arbitrary event payloads or SIP credentials here.
			if event.Type != "status.updated" && event.Type != "recording.updated" && event.Type != "sim-auth.assignment.updated" && event.Type != "contacts.updated" && event.Type != "service.connected" && event.Type != "service.disconnected" && event.Type != "service.activation.updated" && event.Type != "screening.pipeline.updated" {
				continue
			}
			_ = socket.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if socket.WriteJSON(map[string]any{"type": "history.invalidate", "resource": event.Type}) != nil {
				return
			}
		case <-ticker.C:
			if socket.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)) != nil {
				return
			}
		}
	}
}
