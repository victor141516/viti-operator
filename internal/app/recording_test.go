package app

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordingTrackAndSettingSurviveStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	call := Call{ID: "call-one", TraceID: "trace-one", EngineCallID: "engine-one", Instance: "default", StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCallRecordingEnabled(true); err != nil {
		t.Fatal(err)
	}
	track := RecordingTrack{ID: "remote", Role: "remote", Label: "Other party", ContentType: "audio/wav", File: filepath.Join("engine-one", "remote.wav"), CreatedAt: time.Now().UTC()}
	if _, found, err := store.AddRecordingTrack("default", "engine-one", track); err != nil || !found {
		t.Fatalf("add track: found=%v err=%v", found, err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.GetCall("call-one")
	if !ok || len(got.RecordingTracks) != 1 || got.RecordingTracks[0].File != track.File {
		t.Fatalf("persisted recording track = %#v", got.RecordingTracks)
	}
	if !reopened.CallRecordingEnabled() {
		t.Fatal("recording setting was not persisted")
	}
}

func TestMixWAVAlignsTracksAndClipsSamples(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.wav")
	second := filepath.Join(dir, "second.wav")
	if err := os.WriteFile(first, writePCM16WAV(1000, 1, []int16{30000, 30000, -30000}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, writePCM16WAV(1000, 1, []int16{10000, -2000}), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	mixed, err := mixWAV([]recordingInput{{path: first, startedAt: start}, {path: second, startedAt: start.Add(time.Millisecond)}}, start, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(mixed) != 50 {
		t.Fatalf("WAV length = %d, want 50", len(mixed))
	}
	got := []int16{
		int16(binary.LittleEndian.Uint16(mixed[44:46])),
		int16(binary.LittleEndian.Uint16(mixed[46:48])),
		int16(binary.LittleEndian.Uint16(mixed[48:50])),
	}
	want := []int16{30000, 32767, -32000}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("sample %d = %d, want %d", index, got[index], want[index])
		}
	}
}

func TestMixWAVKeepsTheFullCallTimelineForLateAndShortTracks(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short.wav")
	late := filepath.Join(dir, "late.wav")
	if err := os.WriteFile(short, writePCM16WAV(1000, 1, []int16{100, 100, 100}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(late, writePCM16WAV(1000, 1, []int16{200, 200}), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	end := start.Add(10 * time.Millisecond)

	mixed, err := mixWAV([]recordingInput{
		{path: short, startedAt: start},
		{path: late, startedAt: start.Add(5 * time.Millisecond)},
	}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	got := wavSamples(t, mixed)
	want := []int16{100, 100, 100, 0, 0, 200, 200, 0, 0, 0}
	if len(got) != len(want) {
		t.Fatalf("samples = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("sample %d = %d, want %d", index, got[index], want[index])
		}
	}

	lateOnly, err := mixWAV([]recordingInput{{path: late, startedAt: start.Add(5 * time.Millisecond)}}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	got = wavSamples(t, lateOnly)
	want = []int16{0, 0, 0, 0, 0, 200, 200, 0, 0, 0}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("late-only sample %d = %d, want %d", index, got[index], want[index])
		}
	}
}

func TestPCM16WAVDurationReadsAudioLengthWithoutDecodingSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duration.wav")
	if err := os.WriteFile(path, writePCM16WAV(8_000, 1, make([]int16, 12_000)), 0o600); err != nil {
		t.Fatal(err)
	}
	duration, err := pcm16WAVDuration(path)
	if err != nil {
		t.Fatal(err)
	}
	if duration != 1500*time.Millisecond {
		t.Fatalf("duration = %s, want 1.5s", duration)
	}
}

func TestRecordingTrackTimingUsesCallOffsetAndActualWAVDuration(t *testing.T) {
	directory := t.TempDir()
	callID := "call-timing"
	if err := os.Mkdir(filepath.Join(directory, callID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, callID, "late.wav"), writePCM16WAV(8_000, 1, make([]int16, 4_000)), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	application := &App{cfg: Config{Recordings: RecordingsConfig{Directory: directory}}}
	track := application.recordingTrackTiming(
		Call{StartedAt: start},
		RecordingTrack{File: filepath.Join(callID, "late.wav"), CreatedAt: start.Add(5 * time.Second)},
	)
	if track.OffsetMS != 5_000 || track.DurationMS != 500 {
		t.Fatalf("timing = offset %dms, duration %dms; want 5000ms and 500ms", track.OffsetMS, track.DurationMS)
	}
}

func TestRecordingTimingUpdateRetainsTrackAddedAfterSnapshot(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	call := Call{
		ID: "call-concurrent", Instance: "default", EngineCallID: "engine-concurrent",
		StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		RecordingTracks: []RecordingTrack{{ID: "remote", CreatedAt: time.Now().UTC()}},
	}
	if err := store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	// Reproduce the stale-snapshot window: another event appends a client track
	// after the call list was read but before derived timing is persisted.
	_ = store.Calls()
	if _, found, err := store.AddRecordingTrack("default", "engine-concurrent", RecordingTrack{ID: "client-new"}); err != nil || !found {
		t.Fatalf("add concurrent track: found=%v err=%v", found, err)
	}
	if _, found, err := store.UpdateRecordingTrackTiming(call.ID, "remote", 250, 1_500); err != nil || !found {
		t.Fatalf("update timing: found=%v err=%v", found, err)
	}
	got, ok := store.GetCall(call.ID)
	if !ok || len(got.RecordingTracks) != 2 {
		t.Fatalf("recording tracks = %#v; concurrent track was lost", got.RecordingTracks)
	}
	if got.RecordingTracks[0].OffsetMS != 250 || got.RecordingTracks[0].DurationMS != 1_500 {
		t.Fatalf("updated timing = %#v", got.RecordingTracks[0])
	}
}

func wavSamples(t *testing.T, wav []byte) []int16 {
	t.Helper()
	if len(wav) < 44 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatal("invalid WAV data")
	}
	samples := make([]int16, (len(wav)-44)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(wav[44+index*2 : 46+index*2]))
	}
	return samples
}
