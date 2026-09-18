package realtimebot

import (
	"encoding/binary"
	"testing"
)

func TestDecodeWAVToPCMUMixesAndResamplesPCM(t *testing.T) {
	const channels = 2
	const sampleRate = 16000
	frames := sampleRate / 10
	pcm := make([]byte, frames*channels*2)
	positive, negative := int16(1000), int16(-1000)
	for frame := 0; frame < frames; frame++ {
		binary.LittleEndian.PutUint16(pcm[(frame*channels)*2:], uint16(positive))
		binary.LittleEndian.PutUint16(pcm[(frame*channels+1)*2:], uint16(negative))
	}
	wav := testPCM16WAV(channels, sampleRate, pcm)
	result, err := decodeWAVToPCMU(wav)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != pcmuClockRate/10 {
		t.Fatalf("PCMU samples = %d", len(result))
	}
	for _, sample := range result {
		if sample != 0xff {
			t.Fatalf("mixed silence encoded as %#x", sample)
		}
	}
}

func TestDecodeWAVToPCMUAcceptsNativeMuLaw(t *testing.T) {
	payload := []byte{0xff, 0x7f, 0xd5}
	wav := make([]byte, 44+len(payload))
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(len(wav)-8))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 7)
	binary.LittleEndian.PutUint16(wav[22:24], 1)
	binary.LittleEndian.PutUint32(wav[24:28], pcmuClockRate)
	binary.LittleEndian.PutUint32(wav[28:32], pcmuClockRate)
	binary.LittleEndian.PutUint16(wav[32:34], 1)
	binary.LittleEndian.PutUint16(wav[34:36], 8)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(len(payload)))
	copy(wav[44:], payload)
	result, err := decodeWAVToPCMU(wav)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != string(payload) {
		t.Fatalf("native PCMU = %#v", result)
	}
}

func testPCM16WAV(channels, sampleRate int, pcm []byte) []byte {
	wav := make([]byte, 44+len(pcm))
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(len(wav)-8))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(wav[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(wav[28:32], uint32(sampleRate*channels*2))
	binary.LittleEndian.PutUint16(wav[32:34], uint16(channels*2))
	binary.LittleEndian.PutUint16(wav[34:36], 16)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(len(pcm)))
	copy(wav[44:], pcm)
	return wav
}
