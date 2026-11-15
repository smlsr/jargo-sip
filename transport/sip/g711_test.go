package sip

import (
	"bytes"
	"testing"

	"github.com/gojargo/jargo/audio/g711"
)

func TestG711MatchesJargo(t *testing.T) {
	pcm := make([]byte, frameBytes)
	for i := 0; i < len(pcm); i += 2 {
		sample := int16(i*17 - 4000)
		pcm[i] = byte(sample)
		pcm[i+1] = byte(sample >> 8)
	}
	ulaw, err := encodePCM(CodecPCMU, pcm)
	if err != nil {
		t.Fatal(err)
	}
	alaw, err := encodePCM(CodecPCMA, pcm)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ulaw, g711.EncodeULaw(pcm)) || len(ulaw) != frameSamples {
		t.Fatalf("ulaw len %d", len(ulaw))
	}
	if !bytes.Equal(alaw, g711.EncodeALaw(pcm)) || len(alaw) != frameSamples {
		t.Fatalf("alaw len %d", len(alaw))
	}
	if got := g711.DecodeULaw(ulaw); len(got) != frameBytes {
		t.Fatalf("decode ulaw %d", len(got))
	}
	if got := g711.DecodeALaw(alaw); len(got) != frameBytes {
		t.Fatalf("decode alaw %d", len(got))
	}

	if !bytes.Equal(silencePayload(CodecPCMU), bytes.Repeat([]byte{ulawSilence}, frameSamples)) {
		t.Fatal("µ-law silence is not 0xFF")
	}
	if !bytes.Equal(silencePayload(CodecPCMA), bytes.Repeat([]byte{alawSilence}, frameSamples)) {
		t.Fatal("A-law silence is not 0xD5")
	}
}
