package sip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// dtmfVolume is the RFC 4733 volume field. 10 is -10 dBm0.
	dtmfVolume = 10
	// dtmfEndRepeats is the number of end packets after the press.
	dtmfEndRepeats = 3
	// maxDTMFPackets is the longest press whose duration still fits in the
	// 16-bit duration field when each packet adds 160 timestamp units.
	maxDTMFPackets = 409
	maxDTMFQueue   = 32
	maxQueuedDTMF  = 32
)

// DTMF is one telephone-event press.
// A zero Duration on send uses Config.DTMF.DigitDuration.
// On receive, Duration is the duration carried by the packet that completed the press.
type DTMF struct {
	Event    uint8
	Duration time.Duration
}

// ErrDTMFRange is returned when the event is outside the negotiated fmtp set.
// WriteDTMF sends nothing in that case.
var ErrDTMFRange = errors.New("dtmf event is outside the negotiated range")

type dtmfReq struct {
	event   uint8
	packets int
	pt      uint8
}

type dtmfRun struct {
	event   uint8
	pt      uint8
	packets int
	sent    int
	start   uint32
}

type dtmfKey struct {
	ssrc  uint32
	ts    uint32
	event uint8
}

type dtmfPress struct {
	key     dtmfKey
	dur     uint16
	ended   bool
	emitted bool
}

// dtmfRecv turns RFC 4733 packets into one DTMF per press.
type dtmfRecv struct {
	mu     sync.Mutex
	cond   *sync.Cond
	on     bool
	fmtp   string
	cur    *dtmfPress
	done   map[dtmfKey]struct{}
	queue  []DTMF
	closed bool
}

func newDTMFRecv() *dtmfRecv {
	d := &dtmfRecv{fmtp: "0-15", done: map[dtmfKey]struct{}{}}
	d.cond = sync.NewCond(&d.mu)
	return d
}

func (d *dtmfRecv) setSpec(on bool, fmtp string) {
	if d == nil {
		return
	}
	if strings.TrimSpace(fmtp) == "" {
		fmtp = "0-15"
	}
	d.mu.Lock()
	d.on = on
	d.fmtp = fmtp
	d.mu.Unlock()
}

func (d *dtmfRecv) shutdown() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.closed = true
	d.cond.Broadcast()
	d.mu.Unlock()
}

// observe demuxes one telephone-event payload. The caller has already
// consumed the RTP sequence. An event outside fmtp is not emitted.
// The first end bit for (SSRC, timestamp, event) emits one DTMF.
// A new timestamp emits the previous press when that press never set the end bit.
// Retransmitted end packets do not emit again. The marker bit is not consulted.
func (d *dtmfRecv) observe(ssrc, ts uint32, payload []byte) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || !d.on || len(payload) < 4 {
		return
	}
	event := payload[0]
	end := payload[1]&0x80 != 0
	dur := binary.BigEndian.Uint16(payload[2:4])
	key := dtmfKey{ssrc: ssrc, ts: ts, event: event}
	if !fmtpAllows(d.fmtp, event) {
		// The event itself is dropped. Its timestamp still ends an open press.
		if d.cur != nil && (d.cur.key.ssrc != ssrc || d.cur.key.ts != ts) {
			d.finishOpen()
		}
		return
	}
	if d.cur != nil && d.cur.key == key {
		d.cur.dur = dur
		if end {
			d.cur.ended = true
			if !d.cur.emitted {
				d.emit(d.cur)
			}
		}
		return
	}
	// A repeat of a press already emitted is not a new timestamp. Closing the
	// open press here would emit the next digit before its own end.
	if _, seen := d.done[key]; seen {
		return
	}
	d.finishOpen()
	d.cur = &dtmfPress{key: key, dur: dur, ended: end}
	if end {
		d.emit(d.cur)
	}
}

// noteTimestamp completes an open press when a later RTP packet, including
// audio, carries a newer timestamp. An older timestamp is late media and
// leaves the press open. The packet is not itself a digit.
func (d *dtmfRecv) noteTimestamp(ssrc, ts uint32) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || !d.on || d.cur == nil {
		return
	}
	if d.cur.key.ssrc != ssrc || !timestampAfter(ts, d.cur.key.ts) {
		return
	}
	d.finishOpen()
}

// timestampAfter reports whether a is later than b on the RTP clock.
// The comparison is the RFC 3550 signed 32-bit difference, so a wrapped
// timestamp still counts as later and an older one does not.
func timestampAfter(a, b uint32) bool {
	return int32(a-b) > 0
}

func (d *dtmfRecv) finishOpen() {
	if d.cur != nil && !d.cur.emitted {
		d.emit(d.cur)
	}
	d.cur = nil
}

func (d *dtmfRecv) emit(p *dtmfPress) {
	p.emitted = true
	if d.done == nil {
		d.done = map[dtmfKey]struct{}{}
	}
	d.done[p.key] = struct{}{}
	if len(d.queue) == maxDTMFQueue {
		d.queue = d.queue[1:]
	}
	d.queue = append(d.queue, DTMF{
		Event:    p.key.event,
		Duration: time.Duration(p.dur) * time.Second / wireClock,
	})
	d.cond.Broadcast()
}

func (d *dtmfRecv) pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.queue)
}

func (d *dtmfRecv) read() (DTMF, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.queue) == 0 && !d.closed {
		d.cond.Wait()
	}
	if len(d.queue) == 0 {
		return DTMF{}, ErrMediaClosed
	}
	ev := d.queue[0]
	d.queue[0] = DTMF{}
	d.queue = d.queue[1:]
	return ev, nil
}

// fmtpAllows reports whether event is inside an RFC 4733 fmtp list.
// An empty list means 0-15. A single clock other than 8000 never reaches here.
func fmtpAllows(fmtp string, event uint8) bool {
	fmtp = strings.TrimSpace(fmtp)
	if fmtp == "" {
		fmtp = "0-15"
	}
	for _, part := range strings.Split(fmtp, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, ok := eventRange(part)
		if ok && event >= lo && event <= hi {
			return true
		}
	}
	return false
}

func eventRange(s string) (uint8, uint8, bool) {
	loS, hiS, span := strings.Cut(s, "-")
	lo, err := strconv.Atoi(strings.TrimSpace(loS))
	if err != nil || lo < 0 || lo > 255 {
		return 0, 0, false
	}
	hi := lo
	if span {
		n, herr := strconv.Atoi(strings.TrimSpace(hiS))
		if herr != nil || n < lo || n > 255 {
			return 0, 0, false
		}
		hi = n
	}
	return uint8(lo), uint8(hi), true
}

// dtmfSpan is the number of 20 ms packets in the press, before the end packets.
// A zero duration uses digit. Any other duration rounds up to a 20 ms boundary.
func dtmfSpan(d, digit time.Duration) (int, error) {
	if d < 0 {
		return 0, errors.New("dtmf duration is negative")
	}
	if d == 0 {
		d = digit
	}
	if d <= 0 {
		d = 160 * time.Millisecond
	}
	limit := time.Duration(maxDTMFPackets) * tickInterval
	if d > limit {
		return 0, errors.New("dtmf duration is too long")
	}
	if rem := d % tickInterval; rem != 0 {
		d += tickInterval - rem
	}
	n := int(d / tickInterval)
	if n < 1 || n > maxDTMFPackets || n*frameSamples > 0xFFFF {
		return 0, errors.New("dtmf duration is too long")
	}
	return n, nil
}

func gapTicks(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	if rem := d % tickInterval; rem != 0 {
		d += tickInterval - rem
	}
	return int(d / tickInterval)
}

// telephonePayload is one RFC 4733 event. The end bit is the high bit of the
// second octet. Volume occupies the low six bits. Duration is big-endian.
func telephonePayload(event uint8, end bool, volume uint8, duration uint16) []byte {
	b := []byte{event, volume & 0x3f, 0, 0}
	if end {
		b[1] |= 0x80
	}
	binary.BigEndian.PutUint16(b[2:], duration)
	return b
}

func (m *Media) queueDTMF(ev DTMF, urgent bool) error {
	if err := m.alive(); err != nil {
		return err
	}
	m.mu.Lock()
	pt := m.dtmfPT
	fmtp := m.dtmfFMTP
	digit := m.digitDur
	m.mu.Unlock()
	if pt < 0 {
		return ErrDTMFNotNegotiated
	}
	if !fmtpAllows(fmtp, ev.Event) {
		return fmt.Errorf("dtmf event %d: %w", ev.Event, ErrDTMFRange)
	}
	n, err := dtmfSpan(ev.Duration, digit)
	if err != nil {
		return err
	}
	return m.send.enqueueDTMF(dtmfReq{event: ev.Event, packets: n, pt: uint8(pt)}, urgent)
}
