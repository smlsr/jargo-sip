package main

import (
	"encoding/binary"
	"fmt"
)

// pcm16WAV wraps mono little-endian PCM in a 44-byte WAV header.
func pcm16WAV(pcm []byte, sampleRate int) ([]byte, error) {
	if sampleRate <= 0 {
		return nil, fmt.Errorf("wav sample rate %d", sampleRate)
	}
	if len(pcm)%2 != 0 {
		pcm = pcm[:len(pcm)-1]
	}
	out := make([]byte, 44+len(pcm))
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(pcm)))
	copy(out[8:], "WAVE")
	copy(out[12:], "fmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1)
	binary.LittleEndian.PutUint16(out[22:], 1)
	binary.LittleEndian.PutUint32(out[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(out[32:], 2)
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out, nil
}

// wavPCM reads mono 16-bit PCM out of a WAV container.
func wavPCM(wav []byte) (pcm []byte, sampleRate int, err error) {
	if len(wav) < 12 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("not a wav")
	}
	var rate int
	var data []byte
	off := 12
	for off+8 <= len(wav) {
		id := string(wav[off : off+4])
		n := int(binary.LittleEndian.Uint32(wav[off+4 : off+8]))
		off += 8
		if n < 0 || off+n > len(wav) {
			return nil, 0, fmt.Errorf("wav chunk %s overruns the file", id)
		}
		chunk := wav[off : off+n]
		switch id {
		case "fmt ":
			if len(chunk) < 16 {
				return nil, 0, fmt.Errorf("wav fmt chunk is short")
			}
			format := binary.LittleEndian.Uint16(chunk[0:2])
			channels := binary.LittleEndian.Uint16(chunk[2:4])
			rate = int(binary.LittleEndian.Uint32(chunk[4:8]))
			bits := binary.LittleEndian.Uint16(chunk[14:16])
			if format != 1 || channels != 1 || bits != 16 {
				return nil, 0, fmt.Errorf("wav is %d-bit %d-channel format %d", bits, channels, format)
			}
		case "data":
			data = append([]byte(nil), chunk...)
		}
		off += n
		if n%2 == 1 {
			off++
		}
	}
	if rate <= 0 || data == nil {
		return nil, 0, fmt.Errorf("wav is missing fmt or data")
	}
	return data, rate, nil
}
