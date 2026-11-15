package sip

import (
	"sync"
	"time"

	"github.com/pion/rtcp"
)

// reception is the RFC 3550 sequence, loss, and interarrival state copied
// into a report block. It is not a playout buffer.
type reception struct {
	mu            sync.Mutex
	ssrc          uint32
	have          bool
	cycles        uint32
	baseSeq       uint16
	maxSeq        uint16
	received      uint32
	expectedPrior int64
	receivedPrior int64
	epoch         time.Time
	transit       int64
	haveTransit   bool
	jitter        int64
	lsr           uint32
	srAt          time.Time
	haveSR        bool
}

func (r *reception) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ssrc = 0
	r.have = false
	r.cycles = 0
	r.baseSeq = 0
	r.maxSeq = 0
	r.received = 0
	r.expectedPrior = 0
	r.receivedPrior = 0
	r.epoch = time.Time{}
	r.transit = 0
	r.haveTransit = false
	r.jitter = 0
	r.lsr = 0
	r.srAt = time.Time{}
	r.haveSR = false
}

// note records one accepted RTP packet. audio packets update the interarrival
// estimate. Telephone-event still consumes the sequence so it is not loss.
func (r *reception) note(ssrc uint32, seq, ts uint32, at time.Time, audio bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.have && r.ssrc != ssrc {
		return
	}
	if !r.have {
		r.have = true
		r.ssrc = ssrc
		r.baseSeq = uint16(seq)
		r.maxSeq = uint16(seq)
		r.received = 1
		r.epoch = at
		if audio {
			r.seedJitter(ts, at)
		}
		return
	}
	u := uint16(seq)
	delta := u - r.maxSeq
	if delta < 0x8000 {
		if u < r.maxSeq {
			r.cycles += 0x10000
		}
		r.maxSeq = u
	}
	r.received++
	if audio {
		r.seedJitter(ts, at)
	}
}

func (r *reception) seedJitter(ts uint32, at time.Time) {
	if r.epoch.IsZero() {
		r.epoch = at
	}
	elapsed := at.Sub(r.epoch)
	if elapsed < 0 {
		elapsed = 0
	}
	arrival := elapsed.Nanoseconds() * int64(wireClock) / 1e9
	transit := arrival - int64(ts)
	if !r.haveTransit {
		r.transit = transit
		r.haveTransit = true
		return
	}
	d := transit - r.transit
	if d < 0 {
		d = -d
	}
	r.transit = transit
	r.jitter += (d - r.jitter) / 16
	if r.jitter < 0 {
		r.jitter = 0
	}
}

// noteSR stores the middle 32 bits of the peer NTP time for LSR and DLSR.
func (r *reception) noteSR(ssrc uint32, ntp uint64, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.have && r.ssrc != ssrc {
		return
	}
	r.lsr = uint32(ntp >> 16)
	r.srAt = at
	r.haveSR = true
	if !r.have {
		r.ssrc = ssrc
	}
}

// report builds one block when at least one RTP packet has been accepted.
func (r *reception) report(now time.Time) (rtcp.ReceptionReport, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.have || r.received == 0 {
		return rtcp.ReceptionReport{}, false
	}
	extended := r.cycles + uint32(r.maxSeq)
	expected := int64(extended-uint32(r.baseSeq)) + 1
	if expected < 0 {
		expected = 0
	}
	lost := expected - int64(r.received)
	if lost < 0 {
		lost = 0
	}
	if lost > 0xFFFFFF {
		lost = 0xFFFFFF
	}
	expectedInterval := expected - r.expectedPrior
	receivedInterval := int64(r.received) - r.receivedPrior
	lostInterval := expectedInterval - receivedInterval
	var frac uint8
	if expectedInterval > 0 && lostInterval > 0 {
		f := (lostInterval << 8) / expectedInterval
		if f > 255 {
			f = 255
		}
		frac = uint8(f)
	}
	r.expectedPrior = expected
	r.receivedPrior = int64(r.received)

	var delay uint32
	if r.haveSR {
		d := now.Sub(r.srAt)
		if d < 0 {
			d = 0
		}
		units := d.Nanoseconds() * 65536 / 1e9
		if units > 0xFFFFFFFF {
			units = 0xFFFFFFFF
		}
		delay = uint32(units)
	}
	jitter := r.jitter
	if jitter > 0xFFFFFFFF {
		jitter = 0xFFFFFFFF
	}
	return rtcp.ReceptionReport{
		SSRC:               r.ssrc,
		FractionLost:       frac,
		TotalLost:          uint32(lost),
		LastSequenceNumber: extended,
		Jitter:             uint32(jitter),
		LastSenderReport:   r.lsr,
		Delay:              delay,
	}, true
}
