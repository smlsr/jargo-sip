package sip

import (
	"errors"
	"sync"

	"github.com/gojargo/jargo/audio/resample"
)

// Pipeline converts between the 8000 Hz wire clock and the pipeline rates.
// A rate of 0 or 8000 does not build a resampler. Any other rate builds one
// audio/resample stream for that direction, with ClearAfter set to
// resample.NeverClear. There is no intermediate rate.
//
// ToPipeline runs after decode. FromPipeline runs before encode.
// Call and Media PCM stay at 8000 Hz.
type Pipeline struct {
	mu     sync.Mutex
	in     *resample.Resampler
	out    *resample.Resampler
	clears int
}

// NewPipeline builds the edge. inRate is the rate ReadPCM audio is converted
// to. outRate is the rate WritePCM audio arrives from. Zero means 8000.
func NewPipeline(inRate, outRate int) (*Pipeline, error) {
	if inRate < 0 || outRate < 0 {
		return nil, errors.New("pipeline rate is negative")
	}
	if inRate == 0 {
		inRate = wireClock
	}
	if outRate == 0 {
		outRate = wireClock
	}
	p := &Pipeline{}
	if inRate != wireClock {
		in, err := resample.NewWithConfig(wireClock, inRate, 1, resample.Config{ClearAfter: resample.NeverClear})
		if err != nil {
			return nil, err
		}
		p.in = in
	}
	if outRate != wireClock {
		out, err := resample.NewWithConfig(outRate, wireClock, 1, resample.Config{ClearAfter: resample.NeverClear})
		if err != nil {
			return nil, err
		}
		p.out = out
	}
	return p, nil
}

// Resamplers is how many streams were built. Equal clocks contribute none.
func (p *Pipeline) Resamplers() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	if p.in != nil {
		n++
	}
	if p.out != nil {
		n++
	}
	return n
}

// ToPipeline converts 8000 Hz PCM to the input rate. Equal rates copy.
func (p *Pipeline) ToPipeline(pcm []byte) []byte {
	if p == nil {
		return copyBytes(pcm)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return copyBytes(applyRate(pcm, p.in))
}

// FromPipeline converts output-rate PCM to 8000 Hz. Equal rates copy.
func (p *Pipeline) FromPipeline(pcm []byte) []byte {
	if p == nil {
		return copyBytes(pcm)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return copyBytes(applyRate(pcm, p.out))
}

// Clear drops filter history. It runs when the remote address changes.
// Flush is not used to invent samples at the end of a call.
func (p *Pipeline) Clear() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.in != nil {
		p.in.Clear()
	}
	if p.out != nil {
		p.out.Clear()
	}
	p.clears++
}

func (p *Pipeline) clearCount() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clears
}

// applyRate runs pcm through r. A nil resampler leaves the samples unchanged,
// which is the 8000 Hz path. Decode stays in front of this, and encode behind it.
func applyRate(pcm []byte, r *resample.Resampler) []byte {
	if r == nil {
		return pcm
	}
	return r.Process(pcm)
}

func copyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}
