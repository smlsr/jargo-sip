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
	queue        []byte
	armSilence   bool
	inSilence    bool
	forceSilence bool
	closed       bool
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
	if len(s.queue)+len(pcm) > maxPCMQueue {
		return errors.New("pcm queue is full")
	}
	s.queue = append(s.queue, pcm...)
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
func (s *sender) packet() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrMediaClosed
	}
	payload, marker := s.payload()
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    s.pt,
			SequenceNumber: s.seq,
			Timestamp:      s.ts,
			SSRC:           s.ssrc,
		},
		Payload: payload,
	}
	s.lastSeq = s.seq
	s.lastTS = s.ts
	s.seq++
	s.ts += frameSamples
	s.packets++
	s.octets += uint32(len(payload))
	return pkt.Marshal()
}

func (s *sender) payload() ([]byte, bool) {
	var pcm []byte
	if !s.forceSilence && len(s.queue) >= frameBytes {
		pcm = make([]byte, frameBytes)
		copy(pcm, s.queue[:frameBytes])
		rest := make([]byte, len(s.queue)-frameBytes)
		copy(rest, s.queue[frameBytes:])
		s.queue = rest
	}
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
