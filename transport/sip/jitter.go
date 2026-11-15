package sip

import (
	"sync"
	"time"
)

const (
	// jitterRing is one slot per RTP sequence. 128 frames is longer than the
	// 200 ms maximum so a burst can sit ahead of the playout head.
	jitterRing = 128
	adaptEvery = 2 * time.Second
	maxPlayout = 32
)

// jitter is a 20 ms sequence ring. Pion's interceptor jitter buffer is not used.
// Target is how long the playout head waits behind the newest packet.
type jitter struct {
	mu   sync.Mutex
	cond *sync.Cond

	target time.Duration
	min    time.Duration
	max    time.Duration
	codec  Codec

	slots    [jitterRing]slot
	have     bool
	started  bool
	oldest   uint16
	newest   uint16
	head     uint16
	oldestAt time.Time
	prev     []byte
	out      [][]byte

	underruns int
	windowAt  time.Time
	below     bool
	missed    bool
	// hold is set when an underrun raises target. The next pull waits until
	// that much real audio is buffered, or the next packet is older than it.
	hold   bool
	seq    seqState
	far    []farPkt
	closed bool
}

type slot struct {
	occupied bool
	event    bool
	seq      uint16
	samples  int
	pcm      []byte
	at       time.Time
}

// farPkt is one packet outside the ring. One such jump is ignored. A second
// packet in the same range adopts the range.
type farPkt struct {
	ssrc    uint32
	seq     uint16
	samples int
	event   bool
	pcm     []byte
	at      time.Time
}

const farAdopt = 2

type pullKind uint8

const (
	pullNone pullKind = iota
	pullFrame
	pullGap
)

type seqState struct {
	have     bool
	ssrc     uint32
	cycles   uint32
	base     uint16
	max      uint16
	got      uint32
	expPrior int64
	gotPrior int64
}

type lossSnap struct {
	ssrc  uint32
	frac  uint8
	total uint32
	ext   uint32
}

func newJitter(cfg JitterConfig, codec Codec) *jitter {
	j := &jitter{
		target: cfg.Target,
		min:    cfg.Min,
		max:    cfg.Max,
		codec:  codec,
	}
	j.cond = sync.NewCond(&j.mu)
	return j
}

func (j *jitter) setCodec(c Codec) {
	j.mu.Lock()
	j.codec = c
	j.mu.Unlock()
}

// reset drops every packet and any frame waiting on ReadPCM.
// It reports whether audio from the previous address was discarded.
// The adapted target is kept. Loss counts start over with the new source.
func (j *jitter) reset() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	had := j.have || len(j.out) > 0
	for i := range j.slots {
		j.slots[i] = slot{}
	}
	j.have = false
	j.started = false
	j.oldest = 0
	j.newest = 0
	j.head = 0
	j.oldestAt = time.Time{}
	j.prev = nil
	j.out = nil
	j.windowAt = time.Time{}
	j.below = false
	j.missed = false
	j.hold = false
	j.seq = seqState{}
	j.far = nil
	j.cond.Broadcast()
	return had
}

func (j *jitter) shutdown() {
	j.mu.Lock()
	j.closed = true
	j.cond.Broadcast()
	j.mu.Unlock()
}

// pushAudio stores one decoded frame. A packet behind the playout head is
// counted for loss and then dropped. It does not move the head backward.
func (j *jitter) pushAudio(ssrc uint32, seq uint16, pcm []byte, at time.Time) {
	samples := len(pcm) / 2
	if samples != frameSamples && samples != frameSamples/2 {
		return
	}
	j.push(ssrc, seq, pcm, samples, false, at)
}

// pushEvent consumes a telephone-event sequence. It is not loss. The playout
// tick repeats the previous audio frame for that 20 ms and does not underrun.
func (j *jitter) pushEvent(ssrc uint32, seq uint16, at time.Time) {
	j.push(ssrc, seq, nil, 0, true, at)
}

func (j *jitter) push(ssrc uint32, seq uint16, pcm []byte, samples int, event bool, at time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	if j.started && seqLess(seq, j.head) {
		j.seq.note(ssrc, seq)
		return
	}
	if j.have && j.aheadOfRing(seq) {
		j.noteFar(ssrc, seq, pcm, samples, event, at)
		return
	}
	if !j.place(seq) {
		return
	}
	j.abandonFar()
	j.seq.note(ssrc, seq)
	j.store(seq, pcm, samples, event, at)
	j.track(seq, at)
}

// count records a packet that will not be played so the sequence is not loss.
func (j *jitter) count(ssrc uint32, seq uint16) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	j.seq.note(ssrc, seq)
}

// step plays at most one 20 ms frame. Before the start condition it plays nothing.
func (j *jitter) step(now time.Time) ([]byte, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, false
	}
	frame, ok := j.emit(now)
	if !ok {
		return nil, false
	}
	j.enqueue(frame)
	j.cond.Broadcast()
	return frame, true
}

func (j *jitter) read() ([]byte, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for len(j.out) == 0 && !j.closed {
		j.cond.Wait()
	}
	if j.closed {
		return nil, ErrMediaClosed
	}
	f := j.out[0]
	j.out[0] = nil
	j.out = j.out[1:]
	return f, nil
}

func (j *jitter) emit(now time.Time) ([]byte, bool) {
	if !j.have {
		return nil, false
	}
	if !j.started {
		if !j.primed(now) {
			return nil, false
		}
		j.started = true
		j.head = j.oldest
		j.windowAt = now
		j.below = false
		j.missed = false
	}
	// After an underrun the new target is how long the head waits. Do not
	// pull again until that much audio is in front of it.
	if j.hold && !j.rebuilt(now) {
		return nil, false
	}
	frame, kind := j.pull()
	if kind == pullNone {
		return nil, false
	}
	if kind == pullGap {
		j.underruns++
		j.missed = true
		j.raise()
		j.hold = true
	} else {
		j.hold = false
	}
	j.adapt(now)
	return frame, true
}

// primed is Target of audio behind the newest packet, or the oldest packet
// sitting strictly longer than Target. Equal age does not start.
func (j *jitter) primed(now time.Time) bool {
	if j.buffered() >= j.target {
		return true
	}
	return !j.oldestAt.IsZero() && now.Sub(j.oldestAt) > j.target
}

// buffered is the audio actually stored from the oldest packet through the
// newest. A hole adds nothing. 160 samples are 20 ms and 80 samples are 10 ms.
func (j *jitter) buffered() time.Duration {
	if !j.have {
		return 0
	}
	return j.audioFrom(j.oldest, j.newest)
}

// depth is unplayed audio in front of the head, counted the same way.
func (j *jitter) depth() time.Duration {
	if !j.started || !j.have || seqLess(j.newest, j.head) {
		return 0
	}
	return j.audioFrom(j.head, j.newest)
}

// rebuilt reports whether the wait after a raised target is over.
func (j *jitter) rebuilt(now time.Time) bool {
	if j.depth() >= j.target {
		return true
	}
	at, ok := j.nextAt()
	return ok && now.Sub(at) > j.target
}

func (j *jitter) audioFrom(from, until uint16) time.Duration {
	if seqLess(until, from) {
		return 0
	}
	dist := int(until - from)
	if dist > jitterRing {
		dist = jitterRing
	}
	var samples int
	for i := 0; i <= dist; i++ {
		s := j.at(from + uint16(i))
		if s == nil || s.event {
			continue
		}
		samples += s.samples
	}
	return time.Duration(samples) * time.Second / wireClock
}

func (j *jitter) nextAt() (time.Time, bool) {
	if !j.have || seqLess(j.newest, j.head) {
		return time.Time{}, false
	}
	dist := int(j.newest - j.head)
	if dist > jitterRing {
		dist = jitterRing
	}
	for i := 0; i <= dist; i++ {
		s := j.at(j.head + uint16(i))
		if s != nil {
			return s.at, true
		}
	}
	return time.Time{}, false
}

func (j *jitter) pull() ([]byte, pullKind) {
	s := j.at(j.head)
	if s == nil {
		// Nothing was received at head. A newer packet already in the ring is
		// one hole and conceals once. An empty slot past newest is not a gap:
		// leave head where it is so the next real packet is not late.
		if !seqLess(j.head, j.newest) {
			return nil, pullNone
		}
		j.head++
		return j.conceal(), pullGap
	}
	if s.event {
		j.drop(j.head)
		j.head++
		return j.conceal(), pullFrame
	}
	if s.samples == frameSamples {
		frame := j.keep(s.pcm)
		j.drop(j.head)
		j.head++
		return frame, pullFrame
	}
	next := j.at(j.head + 1)
	if next != nil && !next.event && next.samples == frameSamples/2 {
		frame := j.keep(joinPCM(s.pcm, next.pcm))
		j.drop(j.head)
		j.drop(j.head + 1)
		j.head += 2
		return frame, pullFrame
	}
	frame := j.keep(joinPCM(s.pcm, s.pcm))
	j.drop(j.head)
	j.head++
	return frame, pullGap
}

func (j *jitter) conceal() []byte {
	if len(j.prev) == 0 {
		pcm, err := decodePayload(j.codec, silencePayload(j.codec))
		if err != nil || len(pcm) != frameBytes {
			pcm = make([]byte, frameBytes)
		}
		j.prev = pcm
	}
	return append([]byte(nil), j.prev...)
}

func (j *jitter) keep(pcm []byte) []byte {
	j.prev = append([]byte(nil), pcm...)
	return append([]byte(nil), j.prev...)
}

func (j *jitter) raise() {
	next := j.target + tickInterval
	if next > j.max {
		next = j.max
	}
	j.target = next
}

// adapt lowers the target after two seconds spent entirely above Target+20 ms
// with no underrun. An underrun has already raised the target for the next tick.
func (j *jitter) adapt(now time.Time) {
	if j.depth() <= j.target+tickInterval {
		j.below = true
	}
	if j.windowAt.IsZero() {
		j.windowAt = now
	}
	if now.Sub(j.windowAt) < adaptEvery {
		return
	}
	if !j.below && !j.missed {
		next := j.target - tickInterval
		if next < j.min {
			next = j.min
		}
		j.target = next
	}
	j.windowAt = now
	j.below = false
	j.missed = false
}

func (j *jitter) enqueue(frame []byte) {
	if len(j.out) == maxPlayout {
		j.out[0] = nil
		j.out = j.out[1:]
	}
	j.out = append(j.out, frame)
}

func (j *jitter) at(seq uint16) *slot {
	s := &j.slots[int(seq)&(jitterRing-1)]
	if !s.occupied || s.seq != seq {
		return nil
	}
	return s
}

func (j *jitter) drop(seq uint16) {
	i := int(seq) & (jitterRing - 1)
	if j.slots[i].seq == seq {
		j.slots[i] = slot{}
	}
}

func (j *jitter) store(seq uint16, pcm []byte, samples int, event bool, at time.Time) {
	j.slots[int(seq)&(jitterRing-1)] = slot{
		occupied: true,
		event:    event,
		seq:      seq,
		samples:  samples,
		pcm:      append([]byte(nil), pcm...),
		at:       at,
	}
}

// aheadOfRing reports a sequence the ring cannot hold in front of the head.
func (j *jitter) aheadOfRing(seq uint16) bool {
	anchor := j.oldest
	if j.started {
		anchor = j.head
	}
	if seqLess(seq, anchor) || seq == anchor {
		return false
	}
	return uint16(seq-anchor) >= jitterRing
}

// noteFar keeps a jump outside the ring. One packet is a stray and is not
// played. A second packet in that range moves the head and the loss base.
func (j *jitter) noteFar(ssrc uint32, seq uint16, pcm []byte, samples int, event bool, at time.Time) {
	if len(j.far) > 0 && !farSpan(j.far, seq) {
		j.far = nil
	}
	for i := range j.far {
		if j.far[i].seq == seq {
			j.far[i] = farPkt{ssrc: ssrc, seq: seq, samples: samples, event: event, pcm: append([]byte(nil), pcm...), at: at}
			return
		}
	}
	j.far = append(j.far, farPkt{ssrc: ssrc, seq: seq, samples: samples, event: event, pcm: append([]byte(nil), pcm...), at: at})
	if len(j.far) >= farAdopt {
		j.adoptFar()
	}
}

func farSpan(pkts []farPkt, seq uint16) bool {
	base, newest := pkts[0].seq, pkts[0].seq
	for _, p := range pkts[1:] {
		if seqLess(p.seq, base) {
			base = p.seq
		}
		if seqLess(newest, p.seq) {
			newest = p.seq
		}
	}
	if seqLess(seq, base) {
		base = seq
	}
	if seqLess(newest, seq) {
		newest = seq
	}
	return uint16(newest-base) < jitterRing
}

func (j *jitter) abandonFar() {
	j.far = nil
}

func (j *jitter) adoptFar() {
	if len(j.far) < farAdopt {
		return
	}
	for i := range j.slots {
		j.slots[i] = slot{}
	}
	base, newest := j.far[0].seq, j.far[0].seq
	at := j.far[0].at
	ssrc := j.far[0].ssrc
	for _, p := range j.far {
		if seqLess(p.seq, base) {
			base = p.seq
			at = p.at
		}
		if seqLess(newest, p.seq) {
			newest = p.seq
		}
		j.store(p.seq, p.pcm, p.samples, p.event, p.at)
	}
	j.have = true
	j.oldest = base
	j.newest = newest
	j.head = base
	j.oldestAt = at
	j.hold = false
	j.seq.rebase(ssrc, base, newest, uint32(len(j.far)))
	j.far = nil
}

func (j *jitter) place(seq uint16) bool {
	if !j.have {
		return true
	}
	anchor := j.oldest
	if j.started {
		anchor = j.head
	}
	if uint16(seq-anchor) < jitterRing {
		return true
	}
	return !j.started && uint16(j.newest-seq) < jitterRing
}

func (j *jitter) track(seq uint16, at time.Time) {
	if !j.have {
		j.have = true
		j.oldest = seq
		j.newest = seq
		j.head = seq
		j.oldestAt = at
		return
	}
	if !j.started && seqLess(seq, j.oldest) {
		j.oldest = seq
		j.oldestAt = at
	}
	if seqLess(j.newest, seq) {
		j.newest = seq
	}
}

func (j *jitter) loss() (lossSnap, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.seq.snap()
}

func (j *jitter) delay() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.target
}

func (j *jitter) underrunCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.underruns
}

func (j *jitter) packets() uint32 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.seq.got
}

func (j *jitter) running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.started
}

func (st *seqState) rebase(ssrc uint32, base, max uint16, got uint32) {
	*st = seqState{have: true, ssrc: ssrc, base: base, max: max, got: got}
}

func (st *seqState) note(ssrc uint32, seq uint16) {
	if st.have && st.ssrc != ssrc {
		return
	}
	if !st.have {
		st.have = true
		st.ssrc = ssrc
		st.base = seq
		st.max = seq
		st.got = 1
		return
	}
	if seq-st.max < 0x8000 {
		if seq < st.max {
			st.cycles += 0x10000
		}
		st.max = seq
	}
	st.got++
}

func (st *seqState) snap() (lossSnap, bool) {
	if !st.have || st.got == 0 {
		return lossSnap{}, false
	}
	ext := st.cycles + uint32(st.max)
	expected := int64(ext-uint32(st.base)) + 1
	if expected < 0 {
		expected = 0
	}
	lost := expected - int64(st.got)
	if lost < 0 {
		lost = 0
	}
	if lost > 0xFFFFFF {
		lost = 0xFFFFFF
	}
	expInt := expected - st.expPrior
	gotInt := int64(st.got) - st.gotPrior
	lostInt := expInt - gotInt
	var frac uint8
	if expInt > 0 && lostInt > 0 {
		f := (lostInt << 8) / expInt
		if f > 255 {
			f = 255
		}
		frac = uint8(f)
	}
	st.expPrior = expected
	st.gotPrior = int64(st.got)
	return lossSnap{ssrc: st.ssrc, frac: frac, total: uint32(lost), ext: ext}, true
}

func seqLess(a, b uint16) bool {
	return int16(a-b) < 0
}

func joinPCM(a, b []byte) []byte {
	out := make([]byte, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}
