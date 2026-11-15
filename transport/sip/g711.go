package sip

import (
	"fmt"

	"github.com/gojargo/jargo/audio/g711"
)

const (
	wireClock    = 8000
	frameSamples = 160
	frameBytes   = frameSamples * 2
	// maxPCMQueue is two seconds of 16-bit 8000 Hz PCM.
	maxPCMQueue = wireClock * 2 * 2
	ulawSilence = 0xFF
	alawSilence = 0xD5
)

// encodePCM turns 16-bit little-endian PCM into one G.711 byte per sample.
// The bytes match audio/g711 on jargo v0.6.1.
func encodePCM(c Codec, pcm []byte) ([]byte, error) {
	switch c {
	case CodecPCMU:
		return g711.EncodeULaw(pcm), nil
	case CodecPCMA:
		return g711.EncodeALaw(pcm), nil
	default:
		return nil, fmt.Errorf("codec %q is unknown", c)
	}
}

// silencePayload is codec silence, not encoded PCM zeros.
// µ-law silence is 0xFF. A-law silence is 0xD5.
func silencePayload(c Codec) []byte {
	fill := byte(ulawSilence)
	if c == CodecPCMA {
		fill = alawSilence
	}
	out := make([]byte, frameSamples)
	for i := range out {
		out[i] = fill
	}
	return out
}
