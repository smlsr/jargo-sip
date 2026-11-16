package main

import (
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/gojargo/jargo/audio/vad"
)

var errBadRate = errors.New("sample rate must be positive")

// energyVAD is a level detector for phone audio. Silero needs the ONNX
// runtime, which this example does not require. The segmented recognizer
// still needs speech start and stop, and this analyzer supplies them.
type energyVAD struct {
	mu       sync.Mutex
	rate     int
	params   vad.Params
	loudFor  time.Duration
	quietFor time.Duration
	speaking bool
}

func newEnergyVAD() *energyVAD {
	return &energyVAD{params: vad.Params{
		Confidence: 0.5,
		StartSecs:  0.16,
		StopSecs:   0.4,
		// MinVolume is the RMS of the chunk divided by full scale. Phone
		// speech sits well above comfort noise at this level.
		MinVolume: 0.02,
	}}
}

func (e *energyVAD) SetSampleRate(sampleRate int) error {
	if sampleRate <= 0 {
		return errBadRate
	}
	e.mu.Lock()
	e.rate = sampleRate
	e.loudFor = 0
	e.quietFor = 0
	e.speaking = false
	e.mu.Unlock()
	return nil
}

func (e *energyVAD) AnalyzeAudio(buffer []byte) vad.State {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rate <= 0 || len(buffer) < 2 {
		if e.speaking {
			return vad.StateSpeaking
		}
		return vad.StateQuiet
	}
	samples := len(buffer) / 2
	var sum float64
	for i := 0; i+1 < len(buffer); i += 2 {
		v := int16(binary.LittleEndian.Uint16(buffer[i : i+2]))
		f := float64(v)
		sum += f * f
	}
	rms := 0.0
	if samples > 0 {
		mean := sum / float64(samples)
		// Integer square root is enough to compare with a threshold.
		rms = sqrt(mean) / 32768
	}
	dur := time.Duration(samples) * time.Second / time.Duration(e.rate)
	if rms >= e.params.MinVolume {
		e.quietFor = 0
		e.loudFor += dur
		if !e.speaking && e.loudFor >= seconds(e.params.StartSecs) {
			e.speaking = true
		}
	} else {
		e.loudFor = 0
		e.quietFor += dur
		if e.speaking && e.quietFor >= seconds(e.params.StopSecs) {
			e.speaking = false
		}
	}
	if e.speaking {
		return vad.StateSpeaking
	}
	return vad.StateQuiet
}

func (e *energyVAD) Params() vad.Params {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.params
}

func (e *energyVAD) SetParams(params vad.Params) {
	e.mu.Lock()
	e.params = params
	e.loudFor = 0
	e.quietFor = 0
	e.speaking = false
	e.mu.Unlock()
}

func (e *energyVAD) Reset() {
	e.mu.Lock()
	e.loudFor = 0
	e.quietFor = 0
	e.speaking = false
	e.mu.Unlock()
}

func (e *energyVAD) Close() error { return nil }

func seconds(s float64) time.Duration {
	if s < 0 {
		return 0
	}
	return time.Duration(s * float64(time.Second))
}

func sqrt(v float64) float64 {
	if v <= 0 {
		return 0
	}
	g := v
	for i := 0; i < 12; i++ {
		g = (g + v/g) / 2
	}
	return g
}
