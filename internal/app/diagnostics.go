package app

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DiagnosticRecorder deliberately keeps complete, unredacted development
// traces. Its directory is mounted from outside the repository by Compose.
type DiagnosticRecorder struct {
	events  chan []byte
	packets chan []byte
}

func NewDiagnosticRecorder(cfg DiagnosticsConfig) (*DiagnosticRecorder, error) {
	if !cfg.Enabled || cfg.Directory == "" {
		return nil, nil
	}
	session := filepath.Join(cfg.Directory, "sessions", time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.MkdirAll(session, 0o700); err != nil {
		return nil, err
	}
	eventFile, err := os.OpenFile(filepath.Join(session, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	packetFile, err := os.OpenFile(filepath.Join(session, "rtp-relay-packets.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = eventFile.Close()
		return nil, err
	}
	d := &DiagnosticRecorder{events: make(chan []byte, 65536), packets: make(chan []byte, 131072)}
	go writeDiagnosticRecords(eventFile, d.events, 1<<20)
	go writeDiagnosticRecords(packetFile, d.packets, 4<<20)
	return d, nil
}

func writeDiagnosticRecords(file *os.File, records <-chan []byte, bufferSize int) {
	writer := bufio.NewWriterSize(file, bufferSize)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case record := <-records:
			_, _ = writer.Write(record)
			_ = writer.WriteByte('\n')
		case <-ticker.C:
			_ = writer.Flush()
		}
	}
}

func (d *DiagnosticRecorder) RecordEvent(source, kind string, data any) {
	d.RecordTrace(source, kind, "", data)
}

func (d *DiagnosticRecorder) RecordTrace(source, kind, traceID string, data any) {
	if d == nil {
		return
	}
	record := map[string]any{
		"at":       time.Now().UTC(),
		"at_ns":    time.Now().UnixNano(),
		"source":   source,
		"kind":     kind,
		"trace_id": strings.TrimSpace(traceID),
		"data":     data,
	}
	if encoded, err := json.Marshal(record); err == nil {
		d.events <- encoded
	}
}

func (d *DiagnosticRecorder) RecordPacket(port int, direction, source, destination string, payload []byte) {
	if d == nil {
		return
	}
	record := struct {
		At          time.Time `json:"at"`
		Port        int       `json:"port"`
		Direction   string    `json:"direction"`
		Source      string    `json:"source"`
		Destination string    `json:"destination"`
		Length      int       `json:"length"`
		Payload     string    `json:"payload_base64"`
	}{time.Now().UTC(), port, direction, source, destination, len(payload), base64.StdEncoding.EncodeToString(payload)}
	if encoded, err := json.Marshal(record); err == nil {
		d.packets <- encoded
	}
}

func (a *App) diagnosticEvent(w http.ResponseWriter, r *http.Request) {
	if a.diag == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var payload any
	if err := readJSON(r, &payload, 4<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	traceID := ""
	source := "client.voice"
	kind := "event"
	if object, ok := payload.(map[string]any); ok {
		traceID, _ = object["trace_id"].(string)
		if value, ok := object["source"].(string); ok && strings.TrimSpace(value) != "" {
			source = value
		}
		if value, ok := object["kind"].(string); ok && strings.TrimSpace(value) != "" {
			kind = value
		}
	}
	a.diag.RecordTrace(source, kind, traceID, map[string]any{
		"remote_address": r.RemoteAddr,
		"user_agent":     r.UserAgent(),
		"payload":        payload,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprint(w, `{"accepted":true}`)
}

type diagnosticResponseWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *diagnosticResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *diagnosticResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_, _ = w.body.Write(body)
	return w.ResponseWriter.Write(body)
}

func (w *diagnosticResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// traceHTTP is a development flight recorder for every HTTP execution path we
// own. WebSocket frames are captured by PCAP/Asterisk and the upgrade itself is
// traced in voiceWebSocket; wrapping an upgraded connection would break
// http.Hijacker. The diagnostics ingestion endpoint is excluded to avoid
// recursively logging its own writes.
func (a *App) traceHTTP(next http.Handler) http.Handler {
	if a.diag == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sipws" || r.URL.Path == "/api/v1/diagnostics/events" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		requestID, err := randomID("request")
		if err != nil {
			requestID = fmt.Sprintf("request-%d", started.UnixNano())
		}
		body, readErr := io.ReadAll(r.Body)
		if r.Body != nil {
			_ = r.Body.Close()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		traceID := strings.TrimSpace(r.Header.Get("X-Viti-Trace-ID"))
		if traceID == "" && len(body) > 0 {
			var object map[string]any
			if json.Unmarshal(body, &object) == nil {
				traceID, _ = object["trace_id"].(string)
			}
		}
		a.diag.RecordTrace("manager.http", "request.received", traceID, map[string]any{
			"request_id": requestID, "method": r.Method, "url": r.URL.String(),
			"remote_address": r.RemoteAddr, "protocol": r.Proto, "headers": r.Header,
			"body_utf8": string(body), "body_base64": base64.StdEncoding.EncodeToString(body),
			"body_read_error": errorString(readErr),
		})
		writer := &diagnosticResponseWriter{ResponseWriter: w}
		next.ServeHTTP(writer, r)
		if writer.status == 0 {
			writer.status = http.StatusOK
		}
		a.diag.RecordTrace("manager.http", "request.completed", traceID, map[string]any{
			"request_id": requestID, "status": writer.status,
			"duration_ns": time.Since(started).Nanoseconds(), "headers": writer.Header(),
			"body_utf8": writer.body.String(), "body_base64": base64.StdEncoding.EncodeToString(writer.body.Bytes()),
			"context_error": errorString(r.Context().Err()),
		})
	})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
