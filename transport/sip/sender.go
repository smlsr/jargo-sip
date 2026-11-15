package sip

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/rtp"
)

const tickInterval = 20 * time.Millisecond

// ErrMediaClosed is returned when the media session has stopped.
var ErrMediaClosed = errors.New("media session is closed")

// SenderStats is the RTP source identity and the last packet it sent.
type SenderStats struct {
	SSRC          uint32
	Packets       uint32
	Octets        uint32
	LastSequence  uint16
	LastTimestamp uint32
}

type sender struct {
	mu           sync.Mutex
	ssrc         uint32
	seq          uint16
	ts           uint32
	packets      uint32
	octets       uint32
	lastSeq      uint16
	lastTS       uint32
	pt           uint8
	codec        Codec
	items        []sendItem
	run          *dtmfRun
	gap          int
	gapTicks     int
	armSilence   bool
	inSilence    bool
	forceSilence bool
	closed       bool
}

// sendItem is PCM waiting its turn, or one telephone-event press.
// A press replaces audio. PCM queued ahead of a normal press is sent first.
type sendItem struct {
	pcm []byte
	ev  *dtmfReq
}

func newSender(audio Format) (*sender, error) {
	if err := checkAudio(audio); err != nil {
		return nil, err
	}
	ssrc, err := randU32()
	if err != nil {
		return nil, err
	}
	seq, err := randU32()
	if err != nil {
		return nil, err
	}
	ts, err := randU32()
	if err != nil {
		return nil, err
	}
	if ssrc == 0 {
		ssrc = 1
	}
	return &sender{
		ssrc:       ssrc,
		seq:        uint16(seq),
		ts:         ts,
		pt:         uint8(audio.Payload),
		codec:      Codec(audio.Name),
		armSilence: true,
	}, nil
}

func (s *sender) codecName() Codec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codec
}

func (s *sender) setCodec(audio Format) error {
	if err := checkAudio(audio); err != nil {
		return err
	}
	s.mu.Lock()
	s.codec = Codec(audio.Name)
	s.pt = uint8(audio.Payload)
	s.mu.Unlock()
	return nil
}

func (s *sender) setDirection(d Direction) error {
	d, err := normalizeDirection(d)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch d {
	case DirectionSendRecv:
		s.forceSilence = false
	case DirectionSendOnly, DirectionRecvOnly, DirectionInactive:
		if !s.forceSilence {
			s.armSilence = true
		}
		s.forceSilence = true
	default:
		return fmt.Errorf("direction %q is unknown", d)
	}
	return nil
}

func (s *sender) writePCM(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	if len(pcm)%2 != 0 {
		return errors.New("pcm length is odd")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrMediaClosed
	}
	if s.pcmLen()+len(pcm) > maxPCMQueue {
		return errors.New("pcm queue is full")
	}
	if n := len(s.items); n > 0 && s.items[n-1].ev == nil {
		s.items[n-1].pcm = append(s.items[n-1].pcm, pcm...)
		return nil
	}
	s.items = append(s.items, sendItem{pcm: append([]byte(nil), pcm...)})
	return nil
}

func (s *sender) pcmLen() int {
	n := 0
	for _, it := range s.items {
		n += len(it.pcm)
	}
	return n
}

func (s *sender) setGap(ticks int) {
	s.mu.Lock()
	s.gapTicks = ticks
	s.mu.Unlock()
}

// enqueueDTMF schedules one press. Urgent is the next thing the ticker sends,
// ahead of PCM already queued. A normal press waits until that PCM has gone out.
// The press does not start until the current press and its inter-digit gap finish.
func (s *sender) enqueueDTMF(req dtmfReq, urgent bool) error {
	if req.packets < 1 || req.pt > 127 {
		return errors.New("dtmf request is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrMediaClosed
	}
	n := 0
	for _, it := range s.items {
		if it.ev != nil {
			n++
		}
	}
	if n >= maxQueuedDTMF {
		return errors.New("dtmf queue is full")
	}
	saved := req
	item := sendItem{ev: &saved}
	if urgent {
		s.items = append([]sendItem{item}, s.items...)
		s.gap = 0
		return nil
	}
	s.items = append(s.items, item)
	return nil
}

func (s *sender) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *sender) stats() SenderStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SenderStats{
		SSRC:          s.ssrc,
		Packets:       s.packets,
		Octets:        s.octets,
		LastSequence:  s.lastSeq,
		LastTimestamp: s.lastTS,
	}
}

// packet builds the next 20 ms RTP packet. A late caller still gets one packet.
// Telephone-event keeps the timestamp at the start of the press. Audio,
// silence, and the inter-digit gap advance it by 160.
func (s *sender) packet() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrMediaClosed
	}
	if s.run != nil {
		return s.dtmfPacket()
	}
	if s.gap > 0 {
		raw, err := s.emitAudio(nil)
		if err != nil {
			return nil, err
		}
		s.gap--
		return raw, nil
	}
	s.armDTMF()
	if s.run != nil {
		return s.dtmfPacket()
	}
	return s.emitAudio(s.takePCM())
}

func (s *sender) armDTMF() {
	if len(s.items) == 0 || s.items[0].ev == nil {
		return
	}
	req := s.items[0].ev
	s.items = s.items[1:]
	s.run = &dtmfRun{event: req.event, pt: req.pt, packets: req.packets, start: s.ts}
}

// dtmfPacket sends the next slice of the press. The timestamp stays at the
// start. Duration grows by 160 until the press ends, then three end packets
// repeat that duration. The following audio timestamp is the start plus the
// final duration.
func (s *sender) dtmfPacket() ([]byte, error) {
	r := s.run
	end := r.sent >= r.packets
	samples := r.packets * frameSamples
	if !end {
		samples = (r.sent + 1) * frameSamples
	}
	payload := telephonePayload(r.event, end, dtmfVolume, uint16(samples))
	raw, err := s.marshal(r.pt, r.sent == 0, r.start, payload)
	if err != nil {
		return nil, err
	}
	r.sent++
	if r.sent >= r.packets+dtmfEndRepeats {
		s.ts = r.start + uint32(r.packets*frameSamples)
		s.run = nil
		s.inSilence = true
		s.armSilence = true
		if s.gapTicks > 0 && len(s.items) > 0 && s.items[0].ev != nil {
			s.gap = s.gapTicks
		}
	}
	return raw, nil
}

func (s *sender) emitAudio(pcm []byte) ([]byte, error) {
	payload, marker := s.payloadFrom(pcm)
	raw, err := s.marshal(s.pt, marker, s.ts, payload)
	if err != nil {
		return nil, err
	}
	s.ts += frameSamples
	return raw, nil
}

func (s *sender) marshal(pt uint8, marker bool, ts uint32, payload []byte) ([]byte, error) {
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    pt,
			SequenceNumber: s.seq,
			Timestamp:      ts,
			SSRC:           s.ssrc,
		},
		Payload: payload,
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return nil, err
	}
	s.lastSeq = s.seq
	s.lastTS = ts
	s.seq++
	s.packets++
	s.octets += uint32(len(payload))
	return raw, nil
}

// takePCM returns one 20 ms frame from PCM queued ahead of the next press.
// A short tail stays queued. Hold does not consume it.
func (s *sender) takePCM() []byte {
	if s.forceSilence || s.pcmAhead() < frameBytes {
		return nil
	}
	out := make([]byte, 0, frameBytes)
	for len(out) < frameBytes && len(s.items) > 0 && s.items[0].ev == nil {
		need := frameBytes - len(out)
		head := s.items[0].pcm
		if len(head) <= need {
			out = append(out, head...)
			s.items[0] = sendItem{}
			s.items = s.items[1:]
			continue
		}
		out = append(out, head[:need]...)
		s.items[0].pcm = head[need:]
	}
	return out
}

func (s *sender) pcmAhead() int {
	n := 0
	for _, it := range s.items {
		if it.ev != nil {
			break
		}
		n += len(it.pcm)
	}
	return n
}

func (s *sender) payloadFrom(pcm []byte) ([]byte, bool) {
	silence := pcm == nil
	var out []byte
	if silence {
		out = silencePayload(s.codec)
	} else {
		encoded, err := encodePCM(s.codec, pcm)
		if err != nil || len(encoded) != frameSamples {
			out = silencePayload(s.codec)
			silence = true
		} else {
			out = encoded
		}
	}
	marker := false
	if silence {
		if s.armSilence {
			marker = true
			s.armSilence = false
		}
		s.inSilence = true
	} else if s.inSilence {
		marker = true
		s.inSilence = false
		s.armSilence = false
	}
	return out, marker
}

func checkAudio(f Format) error {
	if f.Clock != wireClock {
		return fmt.Errorf("codec clock %d is not 8000", f.Clock)
	}
	switch Codec(f.Name) {
	case CodecPCMU, CodecPCMA:
	default:
		return fmt.Errorf("codec %q is unknown", f.Name)
	}
	if f.Payload < 0 || f.Payload > 127 {
		return fmt.Errorf("payload %d is out of range", f.Payload)
	}
	return nil
}

func eventPayload(dtmf *Format, audio int) int {
	if dtmf == nil {
		return -1
	}
	if dtmf.Clock != wireClock || dtmf.Name != telephoneEvent {
		return -1
	}
	if dtmf.Payload < 0 || dtmf.Payload > 127 || dtmf.Payload == audio {
		return -1
	}
	return dtmf.Payload
}

func randU32() (uint32, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}
