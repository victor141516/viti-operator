package realtimebot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maximumGreetingBytes = 20 << 20

func loadGreetingPCMU(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("open greeting audio: %w", err)
	}
	if info.Size() <= 0 || info.Size() > maximumGreetingBytes {
		return nil, errors.New("greeting audio must contain between 1 byte and 20 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read greeting audio: %w", err)
	}
	extension := strings.ToLower(filepath.Ext(path))
	if extension == ".ulaw" || extension == ".pcmu" {
		return data, nil
	}
	if extension != ".wav" {
		return nil, errors.New("greeting audio must be a .wav, .ulaw, or .pcmu file")
	}
	return decodeWAVToPCMU(data)
}

func decodeWAVToPCMU(data []byte) ([]byte, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("greeting WAV has an invalid RIFF header")
	}
	var format, channels, bitsPerSample uint16
	var sampleRate uint32
	var audio []byte
	for offset := 12; offset+8 <= len(data); {
		chunkID := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if chunkSize < 0 || offset+chunkSize > len(data) {
			return nil, errors.New("greeting WAV contains a truncated chunk")
		}
		chunk := data[offset : offset+chunkSize]
		switch chunkID {
		case "fmt ":
			if len(chunk) < 16 {
				return nil, errors.New("greeting WAV has a truncated format chunk")
			}
			format = binary.LittleEndian.Uint16(chunk[0:2])
			channels = binary.LittleEndian.Uint16(chunk[2:4])
			sampleRate = binary.LittleEndian.Uint32(chunk[4:8])
			bitsPerSample = binary.LittleEndian.Uint16(chunk[14:16])
		case "data":
			audio = chunk
		}
		offset += chunkSize
		if chunkSize%2 != 0 {
			offset++
		}
	}
	if len(audio) == 0 || channels == 0 || sampleRate == 0 {
		return nil, errors.New("greeting WAV is missing audio format or sample data")
	}
	if format == 7 {
		if channels != 1 || sampleRate != pcmuClockRate || bitsPerSample != 8 {
			return nil, errors.New("PCMU WAV greeting must be mono, 8 kHz, and 8-bit")
		}
		return append([]byte(nil), audio...), nil
	}
	if format != 1 || bitsPerSample != 16 {
		return nil, errors.New("WAV greeting must contain 16-bit PCM or 8-bit PCMU audio")
	}
	frameBytes := int(channels) * 2
	if len(audio)%frameBytes != 0 {
		return nil, errors.New("greeting WAV contains an incomplete PCM frame")
	}
	frames := len(audio) / frameBytes
	outputFrames := int((uint64(frames)*pcmuClockRate + uint64(sampleRate) - 1) / uint64(sampleRate))
	if outputFrames == 0 {
		return nil, errors.New("greeting WAV contains no audio frames")
	}
	result := make([]byte, outputFrames)
	for outputIndex := range result {
		sourceIndex := int(uint64(outputIndex) * uint64(sampleRate) / pcmuClockRate)
		if sourceIndex >= frames {
			sourceIndex = frames - 1
		}
		var mixed int32
		frameOffset := sourceIndex * frameBytes
		for channel := 0; channel < int(channels); channel++ {
			sampleOffset := frameOffset + channel*2
			mixed += int32(int16(binary.LittleEndian.Uint16(audio[sampleOffset : sampleOffset+2])))
		}
		result[outputIndex] = linearPCMToMuLaw(int16(mixed / int32(channels)))
	}
	return result, nil
}

func linearPCMToMuLaw(sample int16) byte {
	pcm := int(sample)
	mask := byte(0xff)
	if pcm < 0 {
		pcm = -pcm
		mask = 0x7f
	}
	if pcm > 32635 {
		pcm = 32635
	}
	pcm += 0x84
	segment := 0
	for value := pcm >> 7; value > 1; value >>= 1 {
		segment++
	}
	encoded := byte(segment<<4 | (pcm >> (segment + 3) & 0x0f))
	return encoded ^ mask
}
