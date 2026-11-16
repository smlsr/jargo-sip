package main

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/gojargo/jargo/audio/vad"
)

func TestWAVRoundTrip(t *testing.T) {
	pcm := make([]byte, 8)
	binary.LittleEndian.PutUint16(pcm[0:], 1)
	binary.LittleEndian.PutUint16(pcm[2:], 65535)
	binary.LittleEndian.PutUint16(pcm[4:], 1000)
	binary.LittleEndian.PutUint16(pcm[6:], 0)
	wav, err := pcm16WAV(pcm, 8000)
	if err != nil {
		t.Fatal(err)
	}
	got, rate, err := wavPCM(wav)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 8000 {
		t.Fatalf("rate %d", rate)
	}
	if string(got) != string(pcm) {
		t.Fatalf("pcm %v", got)
	}
}

func TestEnergyVADSpeechThenQuiet(t *testing.T) {
	v := newEnergyVAD()
	if err := v.SetSampleRate(8000); err != nil {
		t.Fatal(err)
	}
	if v.AnalyzeAudio(silence(8000, 300*time.Millisecond)) != vad.StateQuiet {
		t.Fatal("silence was speech")
	}
	if v.AnalyzeAudio(tone(8000, 300*time.Millisecond, 8000)) != vad.StateSpeaking {
		t.Fatal("tone was quiet")
	}
	if v.AnalyzeAudio(silence(8000, 500*time.Millisecond)) != vad.StateQuiet {
		t.Fatal("speech did not end")
	}
}

func silence(rate int, d time.Duration) []byte {
	n := int(d.Seconds() * float64(rate))
	return make([]byte, n*2)
}

func tone(rate int, d time.Duration, amp int16) []byte {
	n := int(d.Seconds() * float64(rate))
	buf := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := amp
		if (i/20)%2 == 1 {
			v = -amp
		}
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	return buf
}
