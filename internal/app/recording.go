package app

import (
	"bytes"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var recordingPathPart = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (a *App) recordingSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, RecordingSettings{Enabled: a.store.CallRecordingEnabled()})
}

func (a *App) setRecordingSettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if err := readJSON(r, &request, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	if err := a.store.SetCallRecordingEnabled(*request.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist call recording preference")
		return
	}
	settings := RecordingSettings{Enabled: *request.Enabled}
	a.events.Publish("recording.updated", settings)
	writeJSON(w, http.StatusOK, settings)
}

func (a *App) engineRecordingSettings(w http.ResponseWriter, r *http.Request) {
	if !a.engineRequestAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, RecordingSettings{Enabled: a.store.CallRecordingEnabled()})
}

func (a *App) engineRequestAuthorized(r *http.Request) bool {
	token := strings.TrimSpace(a.cfg.Voice.EngineEventToken)
	provided := r.Header.Get("X-Viti-Engine-Token")
	return token != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func (a *App) ingestRecordingTrack(w http.ResponseWriter, instance, callID string, args []string, recordedAtNS int64) {
	if len(args) < 4 {
		writeError(w, http.StatusBadRequest, "invalid recording track event")
		return
	}
	role := strings.TrimSpace(args[0])
	participantID := strings.TrimSpace(args[1])
	clientID := strings.TrimSpace(args[2])
	fileName := strings.TrimSpace(args[3])
	segmentID := ""
	if len(args) > 4 {
		segmentID = strings.TrimSpace(args[4])
	}
	if role != "remote" && role != "client" && role != "service" {
		writeError(w, http.StatusBadRequest, "invalid recording track role")
		return
	}
	if !recordingPathPart.MatchString(callID) || !recordingPathPart.MatchString(fileName) || filepath.Base(fileName) != fileName || (segmentID != "" && !recordingPathPart.MatchString(segmentID)) {
		writeError(w, http.StatusBadRequest, "invalid recording track path")
		return
	}
	createdAt := time.Now().UTC()
	if recordedAtNS > 0 {
		createdAt = time.Unix(0, recordedAtNS).UTC()
	}
	label := "Other party"
	trackID := "remote"
	if role == "client" {
		label = a.knownClientName(clientID)
		if label == "" {
			label = "Connected device"
		}
		trackID = "client-" + participantID
		if segmentID != "" {
			trackID += "-" + segmentID
		}
	}
	if role == "service" {
		label = clientID
		trackID = "service-" + participantID
		if segmentID != "" {
			trackID += "-" + segmentID
		}
	}
	track := RecordingTrack{
		ID: trackID, Role: role, ParticipantID: participantID, ClientID: clientID,
		Label: label, ContentType: "audio/wav", File: filepath.Join(callID, fileName), CreatedAt: createdAt,
	}
	call, found, err := a.store.AddRecordingTrack(instance, callID, track)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist recording track")
		return
	}
	if !found {
		writeError(w, http.StatusConflict, "call is not available yet")
		return
	}
	a.events.Publish("call.updated", call)
	writeJSON(w, http.StatusOK, call)
}

func (a *App) claimRecordingParticipant(w http.ResponseWriter, r *http.Request) {
	var request struct {
		TraceID       string `json:"trace_id"`
		ParticipantID string `json:"participant_id"`
		ClientID      string `json:"client_id"`
		Name          string `json:"name"`
	}
	if err := readJSON(r, &request, 8<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.TraceID = strings.TrimSpace(request.TraceID)
	request.ParticipantID = strings.TrimSpace(request.ParticipantID)
	request.ClientID = strings.TrimSpace(request.ClientID)
	request.Name = strings.TrimSpace(request.Name)
	if request.TraceID == "" || request.ParticipantID == "" || request.ClientID == "" {
		writeError(w, http.StatusBadRequest, "trace_id, participant_id, and client_id are required")
		return
	}
	if request.Name == "" {
		request.Name = a.knownClientName(request.ClientID)
	}
	if request.Name == "" {
		request.Name = "Connected device"
	}
	if existing, found := a.store.GetCallByTraceID(request.TraceID); found && existing.EndedAt != nil {
		writeError(w, http.StatusGone, "call has ended")
		return
	}
	// Record the control-plane identity even if the durable call-opening
	// event is still waiting in the engine outbox. The client retries this
	// endpoint so recording metadata converges once the call row exists.
	if !a.callSessions.claim(request.TraceID, request.ParticipantID, request.ClientID) {
		writeError(w, http.StatusGone, "call has ended")
		return
	}
	call, found, err := a.store.ClaimRecordingParticipant(request.TraceID, RecordingParticipant{
		ParticipantID: request.ParticipantID, ClientID: request.ClientID, Name: request.Name,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist recording participant")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "call not found")
		return
	}
	a.events.Publish("call.updated", call)
	writeJSON(w, http.StatusOK, call)
}

func (a *App) knownClientName(clientID string) string {
	if clientID == "" {
		return ""
	}
	for _, client := range a.store.KnownClients() {
		if client.ID == clientID {
			return client.Name
		}
	}
	return ""
}

func (a *App) callRecording(w http.ResponseWriter, r *http.Request) {
	call, ok := a.store.GetCall(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "call not found")
		return
	}
	selected := strings.Split(strings.TrimSpace(r.URL.Query().Get("tracks")), ",")
	if len(selected) == 1 && selected[0] == "" {
		selected = make([]string, 0, len(call.RecordingTracks))
		for _, track := range call.RecordingTracks {
			selected = append(selected, track.ID)
		}
	}
	selectedSet := make(map[string]struct{}, len(selected))
	for _, id := range selected {
		if id = strings.TrimSpace(id); id != "" {
			selectedSet[id] = struct{}{}
		}
	}
	tracks := make([]recordingInput, 0, len(selectedSet))
	for _, track := range call.RecordingTracks {
		if _, wanted := selectedSet[track.ID]; !wanted {
			continue
		}
		path, err := a.recordingPath(track.File)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid stored recording path")
			return
		}
		tracks = append(tracks, recordingInput{path: path, startedAt: track.CreatedAt})
	}
	if len(tracks) == 0 || len(tracks) != len(selectedSet) {
		writeError(w, http.StatusNotFound, "recording tracks are not available")
		return
	}
	for _, track := range tracks {
		if _, err := os.Stat(track.path); err != nil {
			writeError(w, http.StatusNotFound, "recording audio is not available yet")
			return
		}
	}
	name := fmt.Sprintf("viti-call-%s.wav", safeDownloadName(call.ID))
	w.Header().Set("Content-Type", "audio/wav")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	}
	timelineEnd := time.Time{}
	if call.EndedAt != nil {
		timelineEnd = *call.EndedAt
	}
	audio, err := mixWAV(tracks, call.StartedAt, timelineEnd)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	http.ServeContent(w, r, name, time.Now(), bytes.NewReader(audio))
}

func (a *App) recordingPath(relative string) (string, error) {
	root, err := filepath.Abs(a.cfg.Recordings.Directory)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(root, filepath.Clean(relative)))
	if err != nil || (path != root && !strings.HasPrefix(path, root+string(os.PathSeparator))) {
		return "", errors.New("recording path escapes storage root")
	}
	return path, nil
}

func safeDownloadName(value string) string {
	value = regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(value, "-")
	return strings.Trim(value, "-.")
}

type recordingInput struct {
	path      string
	startedAt time.Time
}

type pcmWAV struct {
	sampleRate uint32
	channels   uint16
	bits       uint16
	samples    []int16
}

// pcm16WAVDuration reads only the RIFF chunk headers needed to determine the
// duration. This keeps call-list requests cheap even for long recordings.
func pcm16WAVDuration(path string) (time.Duration, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	header := make([]byte, 12)
	if _, err := io.ReadFull(file, header); err != nil || string(header[:4]) != "RIFF" || string(header[8:]) != "WAVE" {
		return 0, errors.New("recording is not a RIFF/WAVE file")
	}
	var byteRate uint32
	var dataSize uint32
	for {
		chunk := make([]byte, 8)
		if _, err := io.ReadFull(file, chunk); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, err
		}
		size := binary.LittleEndian.Uint32(chunk[4:])
		payloadStart, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
		if int64(size) > info.Size()-payloadStart {
			return 0, errors.New("recording contains a truncated WAVE chunk")
		}
		switch string(chunk[:4]) {
		case "fmt ":
			if size < 16 {
				return 0, errors.New("recording contains an invalid format chunk")
			}
			format := make([]byte, 16)
			if _, err := io.ReadFull(file, format); err != nil {
				return 0, err
			}
			if binary.LittleEndian.Uint16(format[:2]) != 1 {
				return 0, errors.New("recording must use uncompressed PCM")
			}
			byteRate = binary.LittleEndian.Uint32(format[8:12])
			if _, err := file.Seek(int64(size)-16+int64(size%2), io.SeekCurrent); err != nil {
				return 0, err
			}
		case "data":
			dataSize = size
			if _, err := file.Seek(int64(size)+int64(size%2), io.SeekCurrent); err != nil {
				return 0, err
			}
		default:
			if _, err := file.Seek(int64(size)+int64(size%2), io.SeekCurrent); err != nil {
				return 0, err
			}
		}
		if byteRate > 0 && dataSize > 0 {
			return time.Duration(dataSize) * time.Second / time.Duration(byteRate), nil
		}
	}
	return 0, errors.New("recording does not contain duration metadata")
}

func (a *App) recordingTrackTiming(call Call, track RecordingTrack) RecordingTrack {
	offset := track.CreatedAt.Sub(call.StartedAt)
	if offset < 0 {
		offset = 0
	}
	track.OffsetMS = offset.Milliseconds()
	if track.DurationMS <= 0 {
		path, err := a.recordingPath(track.File)
		if err == nil {
			duration, err := pcm16WAVDuration(path)
			if err == nil {
				track.DurationMS = (duration.Nanoseconds() + int64(time.Millisecond) - 1) / int64(time.Millisecond)
			}
		}
	}
	if track.Role == "remote" && call.EndedAt != nil && track.DurationMS > 0 {
		// The carrier MixMonitor hook is installed before the INVITE so it can
		// capture early media. Asterisk does not write leading silence before
		// the first media frame, however, so the hook timestamp is not the
		// beginning of the WAV. The carrier channel and its recording both end
		// with the call; align the finalized file from that reliable boundary.
		callDuration := call.EndedAt.Sub(call.StartedAt)
		recordingDuration := time.Duration(track.DurationMS) * time.Millisecond
		if callDuration >= recordingDuration {
			track.OffsetMS = (callDuration - recordingDuration).Milliseconds()
		}
	}
	return track
}

func (a *App) callsWithRecordingTiming() []Call {
	calls := a.store.Calls()
	for callIndex := range calls {
		calls[callIndex].RecordingTracks = append([]RecordingTrack(nil), calls[callIndex].RecordingTracks...)
		for trackIndex, track := range calls[callIndex].RecordingTracks {
			timed := a.recordingTrackTiming(calls[callIndex], track)
			calls[callIndex].RecordingTracks[trackIndex] = timed
			if calls[callIndex].EndedAt != nil && (timed.OffsetMS != track.OffsetMS || timed.DurationMS != track.DurationMS) {
				// Persist only the derived fields. Async recording/participant events
				// may have added data since Calls returned this snapshot.
				_, _, _ = a.store.UpdateRecordingTrackTiming(calls[callIndex].ID, track.ID, timed.OffsetMS, timed.DurationMS)
			}
		}
	}
	return calls
}

func readPCM16WAV(path string) (pcmWAV, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pcmWAV{}, err
	}
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return pcmWAV{}, errors.New("recording is not a RIFF/WAVE file")
	}
	var result pcmWAV
	var raw []byte
	for offset := 12; offset+8 <= len(data); {
		kind := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start, end := offset+8, offset+8+size
		if end > len(data) {
			return pcmWAV{}, errors.New("recording contains a truncated WAVE chunk")
		}
		switch kind {
		case "fmt ":
			if size < 16 || binary.LittleEndian.Uint16(data[start:start+2]) != 1 {
				return pcmWAV{}, errors.New("recording must use uncompressed PCM")
			}
			result.channels = binary.LittleEndian.Uint16(data[start+2 : start+4])
			result.sampleRate = binary.LittleEndian.Uint32(data[start+4 : start+8])
			result.bits = binary.LittleEndian.Uint16(data[start+14 : start+16])
		case "data":
			raw = data[start:end]
		}
		offset = end + size%2
	}
	if result.bits != 16 || result.channels == 0 || result.sampleRate == 0 || len(raw)%2 != 0 {
		return pcmWAV{}, errors.New("recording must be 16-bit PCM")
	}
	result.samples = make([]int16, len(raw)/2)
	for index := range result.samples {
		result.samples[index] = int16(binary.LittleEndian.Uint16(raw[index*2 : index*2+2]))
	}
	return result, nil
}

func mixWAV(inputs []recordingInput, timelineStart, timelineEnd time.Time) ([]byte, error) {
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].startedAt.Before(inputs[j].startedAt) })
	start := timelineStart
	if start.IsZero() || inputs[0].startedAt.Before(start) {
		start = inputs[0].startedAt
	}
	decoded := make([]pcmWAV, len(inputs))
	maxSamples := 0
	for index, input := range inputs {
		wav, err := readPCM16WAV(input.path)
		if err != nil {
			return nil, fmt.Errorf("read recording track: %w", err)
		}
		if index > 0 && (wav.sampleRate != decoded[0].sampleRate || wav.channels != decoded[0].channels || wav.bits != decoded[0].bits) {
			return nil, errors.New("selected recording tracks use incompatible PCM formats")
		}
		decoded[index] = wav
		offset := recordingSampleOffset(input.startedAt.Sub(start), wav.sampleRate, wav.channels)
		if offset+len(wav.samples) > maxSamples {
			maxSamples = offset + len(wav.samples)
		}
	}
	if timelineEnd.After(start) {
		timelineSamples := recordingSampleLength(timelineEnd.Sub(start), decoded[0].sampleRate, decoded[0].channels)
		if timelineSamples > maxSamples {
			maxSamples = timelineSamples
		}
	}
	mixed := make([]int32, maxSamples)
	for index, wav := range decoded {
		offset := recordingSampleOffset(inputs[index].startedAt.Sub(start), wav.sampleRate, wav.channels)
		for sampleIndex, sample := range wav.samples {
			mixed[offset+sampleIndex] += int32(sample)
		}
	}
	output := make([]int16, len(mixed))
	for index, sample := range mixed {
		if sample > 32767 {
			sample = 32767
		} else if sample < -32768 {
			sample = -32768
		}
		output[index] = int16(sample)
	}
	return writePCM16WAV(decoded[0].sampleRate, decoded[0].channels, output), nil
}

func recordingSampleOffset(delta time.Duration, sampleRate uint32, channels uint16) int {
	if delta <= 0 {
		return 0
	}
	frames := delta.Nanoseconds() * int64(sampleRate) / int64(time.Second)
	return int(frames * int64(channels))
}

func recordingSampleLength(delta time.Duration, sampleRate uint32, channels uint16) int {
	if delta <= 0 {
		return 0
	}
	numerator := delta.Nanoseconds() * int64(sampleRate)
	frames := (numerator + int64(time.Second) - 1) / int64(time.Second)
	return int(frames * int64(channels))
}

func writePCM16WAV(sampleRate uint32, channels uint16, samples []int16) []byte {
	dataSize := uint32(len(samples) * 2)
	buffer := bytes.NewBuffer(make([]byte, 0, int(dataSize)+44))
	_, _ = io.WriteString(buffer, "RIFF")
	_ = binary.Write(buffer, binary.LittleEndian, uint32(36)+dataSize)
	_, _ = io.WriteString(buffer, "WAVEfmt ")
	_ = binary.Write(buffer, binary.LittleEndian, uint32(16))
	_ = binary.Write(buffer, binary.LittleEndian, uint16(1))
	_ = binary.Write(buffer, binary.LittleEndian, channels)
	_ = binary.Write(buffer, binary.LittleEndian, sampleRate)
	_ = binary.Write(buffer, binary.LittleEndian, sampleRate*uint32(channels)*2)
	_ = binary.Write(buffer, binary.LittleEndian, channels*2)
	_ = binary.Write(buffer, binary.LittleEndian, uint16(16))
	_, _ = io.WriteString(buffer, "data")
	_ = binary.Write(buffer, binary.LittleEndian, dataSize)
	for _, sample := range samples {
		_ = binary.Write(buffer, binary.LittleEndian, sample)
	}
	return buffer.Bytes()
}
