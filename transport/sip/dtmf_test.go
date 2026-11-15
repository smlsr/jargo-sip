package sip

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/gojargo/jargo/audio/g711"
	"github.com/pion/rtp"
)

func newSession(t *testing.T, cfg Config, shape func(port int) RemoteMedia) (*Media, *net.UDPConn) {
	t.Helper()
	ep := startEP(t, cfg)
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sock := listenLoop(t)
	port := sock.LocalAddr().(*net.UDPAddr).Port
	if shape == nil {
		shape = func(port int) RemoteMedia {
			return pcmuMedia("127.0.0.1", port, port+1)
		}
	}
	m, err := StartMedia(context.Background(), pair, shape(port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, sock
}

func awaitDTMF(t *testing.T, m *Media, d time.Duration) (DTMF, bool) {
	t.Helper()
	type res struct {
		ev  DTMF
		err error
	}
	ch := make(chan res, 1)
	go func() {
		ev, err := m.ReadDTMF()
		ch <- res{ev, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.ev, true
	case <-time.After(d):
		return DTMF{}, false
	}
}

func awaitPCM(t *testing.T, m *Media, d time.Duration) []byte {
	t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := m.ReadPCM()
		ch <- res{b, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.b
	case <-time.After(d):
		t.Fatal("ReadPCM timed out")
	}
	return nil
}

func injectAt(t *testing.T, m *Media, raw []byte) {
	t.Helper()
	m.takeRTP(raw, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 41000}, time.Now())
}

func eventPkt(t *testing.T, seq uint16, ts uint32, pt, event uint8, end bool, dur uint16, marker bool) []byte {
	t.Helper()
	raw, err := (&rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    pt,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           42,
		},
		Payload: telephonePayload(event, end, dtmfVolume, dur),
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testSender(t *testing.T) *sender {
	t.Helper()
	s, err := newSender(Format{Payload: 0, Name: string(CodecPCMU), Clock: wireClock})
	if err != nil {
		t.Fatal(err)
	}
	s.setGap(gapTicks(100 * time.Millisecond))
	return s
}

func pull(t *testing.T, s *sender) *rtp.Packet {
	t.Helper()
	raw, err := s.packet()
	if err != nil {
		t.Fatal(err)
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	return pkt
}

func TestFMTPRange(t *testing.T) {
	if !fmtpAllows("", 0) || !fmtpAllows("", 15) || fmtpAllows("", 16) {
		t.Fatal("empty fmtp is 0-15")
	}
	if !fmtpAllows("0-16", 16) || !fmtpAllows("0-15,16", 16) {
		t.Fatal("flash is allowed when fmtp lists 16")
	}
	if fmtpAllows("0-15", 16) || !fmtpAllows("0-15", 12) {
		t.Fatal("A-D stays inside 0-15 and flash does not")
	}
}

func TestDTMFSpan(t *testing.T) {
	n, err := dtmfSpan(0, 160*time.Millisecond)
	if err != nil || n != 8 {
		t.Fatalf("default %d %v", n, err)
	}
	n, err = dtmfSpan(30*time.Millisecond, 160*time.Millisecond)
	if err != nil || n != 2 {
		t.Fatalf("round up %d %v", n, err)
	}
	n, err = dtmfSpan(20*time.Millisecond, time.Second)
	if err != nil || n != 1 {
		t.Fatalf("exact %d %v", n, err)
	}
	n, err = dtmfSpan(160*time.Millisecond, time.Second)
	if err != nil || n != 8 {
		t.Fatalf("explicit %d %v", n, err)
	}
	if _, err := dtmfSpan(-time.Millisecond, 160*time.Millisecond); err == nil {
		t.Fatal("negative duration was accepted")
	}
}

func TestWriteDTMFDefaultPress(t *testing.T) {
	s := testSender(t)
	if err := s.enqueueDTMF(dtmfReq{event: 5, packets: 8, pt: 101}, false); err != nil {
		t.Fatal(err)
	}
	pcm := bytes.Repeat([]byte{0x01, 0x02}, frameSamples)
	if err := s.writePCM(pcm); err != nil {
		t.Fatal(err)
	}
	var start uint32
	var seq uint16
	for i := 0; i < 11; i++ {
		pkt := pull(t, s)
		if pkt.PayloadType != 101 || len(pkt.Payload) != 4 || pkt.Payload[0] != 5 {
			t.Fatalf("packet %d %+v %x", i, pkt.Header, pkt.Payload)
		}
		if pkt.Payload[1]&0x3f != dtmfVolume {
			t.Fatalf("volume %d", pkt.Payload[1])
		}
		end := i >= 8
		if (pkt.Payload[1]&0x80 != 0) != end {
			t.Fatalf("end bit packet %d flags %02x", i, pkt.Payload[1])
		}
		wantDur := uint16((i + 1) * frameSamples)
		if end {
			wantDur = 8 * frameSamples
		}
		if got := binary.BigEndian.Uint16(pkt.Payload[2:]); got != wantDur {
			t.Fatalf("duration packet %d = %d", i, got)
		}
		if i == 0 {
			start = pkt.Timestamp
			seq = pkt.SequenceNumber
			if !pkt.Marker {
				t.Fatal("first event packet had no marker")
			}
		} else {
			if pkt.Timestamp != start {
				t.Fatalf("timestamp moved at packet %d", i)
			}
			if pkt.SequenceNumber != seq+uint16(i) {
				t.Fatalf("sequence %d", pkt.SequenceNumber)
			}
			if pkt.Marker {
				t.Fatalf("marker stayed set on packet %d", i)
			}
		}
	}
	audio := pull(t, s)
	if audio.PayloadType != 0 || audio.Timestamp != start+8*frameSamples {
		t.Fatalf("next audio ts %d start %d pt %d", audio.Timestamp, start, audio.PayloadType)
	}
	if audio.SequenceNumber != seq+11 {
		t.Fatalf("audio sequence %d", audio.SequenceNumber)
	}
	if !audio.Marker {
		t.Fatal("audio after the event had no marker")
	}
	want, err := encodePCM(CodecPCMU, pcm)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(audio.Payload, want) {
		t.Fatal("queued PCM was not the next audio")
	}
}

func TestWriteDTMFRoundsUp(t *testing.T) {
	s := testSender(t)
	if err := s.enqueueDTMF(dtmfReq{event: 1, packets: 2, pt: 101}, false); err != nil {
		t.Fatal(err)
	}
	var start uint32
	for i := 0; i < 5; i++ {
		pkt := pull(t, s)
		if i == 0 {
			start = pkt.Timestamp
		}
		want := uint16(frameSamples)
		if i > 0 {
			want = 2 * frameSamples
		}
		if got := binary.BigEndian.Uint16(pkt.Payload[2:]); got != want || pkt.Timestamp != start {
			t.Fatalf("packet %d dur %d ts %d", i, got, pkt.Timestamp)
		}
	}
	next := pull(t, s)
	if next.Timestamp != start+2*frameSamples || next.PayloadType != 0 {
		t.Fatalf("next %+v", next.Header)
	}
}

func TestInterDigitGap(t *testing.T) {
	s := testSender(t)
	if err := s.enqueueDTMF(dtmfReq{event: 5, packets: 1, pt: 101}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.enqueueDTMF(dtmfReq{event: 6, packets: 1, pt: 101}, false); err != nil {
		t.Fatal(err)
	}
	first := pull(t, s)
	start := first.Timestamp
	for i := 0; i < 3; i++ {
		pkt := pull(t, s)
		if pkt.Timestamp != start || pkt.Payload[1]&0x80 == 0 {
			t.Fatalf("end %d %+v %x", i, pkt.Header, pkt.Payload)
		}
	}
	for i := 0; i < 5; i++ {
		pkt := pull(t, s)
		if pkt.PayloadType != 0 || pkt.Timestamp != start+uint32(i+1)*frameSamples {
			t.Fatalf("gap %d ts %d pt %d", i, pkt.Timestamp, pkt.PayloadType)
		}
		if !bytes.Equal(pkt.Payload, bytes.Repeat([]byte{ulawSilence}, frameSamples)) {
			t.Fatal("gap was not codec silence")
		}
	}
	second := pull(t, s)
	wantTS := start + frameSamples + 5*frameSamples
	if second.Payload[0] != 6 || second.Timestamp != wantTS || second.PayloadType != 101 {
		t.Fatalf("second digit %+v %x", second.Header, second.Payload)
	}
}

func TestUrgentDTMFCutsAheadOfPCM(t *testing.T) {
	s := testSender(t)
	pcm := bytes.Repeat([]byte{0x03, 0x04}, frameSamples)
	if err := s.writePCM(pcm); err != nil {
		t.Fatal(err)
	}
	if err := s.enqueueDTMF(dtmfReq{event: 9, packets: 1, pt: 97}, true); err != nil {
		t.Fatal(err)
	}
	pkt := pull(t, s)
	if pkt.PayloadType != 97 || pkt.Payload[0] != 9 {
		t.Fatalf("urgent did not cut in %+v %x", pkt.Header, pkt.Payload)
	}
	for i := 0; i < 3; i++ {
		pull(t, s)
	}
	audio := pull(t, s)
	if audio.PayloadType != 0 || len(audio.Payload) != frameSamples {
		t.Fatalf("pcm %+v", audio.Header)
	}
}

func TestNormalDTMFWaitsForPCM(t *testing.T) {
	s := testSender(t)
	pcm := bytes.Repeat([]byte{0x05, 0x06}, frameSamples)
	if err := s.writePCM(pcm); err != nil {
		t.Fatal(err)
	}
	if err := s.enqueueDTMF(dtmfReq{event: 4, packets: 1, pt: 101}, false); err != nil {
		t.Fatal(err)
	}
	audio := pull(t, s)
	if audio.PayloadType != 0 {
		t.Fatalf("pcm did not go first, pt %d", audio.PayloadType)
	}
	digit := pull(t, s)
	if digit.PayloadType != 101 || digit.Payload[0] != 4 {
		t.Fatalf("digit %+v %x", digit.Header, digit.Payload)
	}
}

func TestReadDTMFThreeEndPackets(t *testing.T) {
	m, _ := newSession(t, baseConfig(), nil)
	for i := 0; i < 3; i++ {
		injectAt(t, m, eventPkt(t, uint16(10+i), 8000, 101, 5, true, 1280, false))
	}
	ev, ok := awaitDTMF(t, m, time.Second)
	if !ok || ev.Event != 5 || ev.Duration != 160*time.Millisecond {
		t.Fatalf("event %+v ok %v", ev, ok)
	}
	if m.tones.pending() != 0 {
		t.Fatal("retransmitted end packets emitted again")
	}
}

func TestReadDTMFPressThenEnds(t *testing.T) {
	m, _ := newSession(t, baseConfig(), nil)
	for i := 0; i < 8; i++ {
		injectAt(t, m, eventPkt(t, uint16(i+1), 8000, 101, 5, false, uint16((i+1)*160), false))
	}
	for i := 0; i < 3; i++ {
		injectAt(t, m, eventPkt(t, uint16(9+i), 8000, 101, 5, true, 1280, false))
	}
	ev, ok := awaitDTMF(t, m, time.Second)
	if !ok || ev.Event != 5 || ev.Duration != 160*time.Millisecond {
		t.Fatalf("event %+v ok %v", ev, ok)
	}
	if m.tones.pending() != 0 {
		t.Fatal("end retransmits emitted again")
	}
}

func TestReadDTMFNoEndBitEmitsOnNextTimestamp(t *testing.T) {
	m, _ := newSession(t, baseConfig(), nil)
	for i := 0; i < 4; i++ {
		injectAt(t, m, eventPkt(t, uint16(i+1), 5000, 101, 5, false, uint16((i+1)*160), false))
	}
	if m.tones.pending() != 0 {
		t.Fatal("emitted before the end bit or a new timestamp")
	}
	injectAt(t, m, eventPkt(t, 20, 9000, 101, 9, false, 160, false))
	ev, ok := awaitDTMF(t, m, time.Second)
	if !ok || ev.Event != 5 || ev.Duration != 80*time.Millisecond {
		t.Fatalf("event %+v ok %v", ev, ok)
	}
	if m.tones.pending() != 0 {
		t.Fatal("digit 5 was emitted again")
	}
}

func TestReadDTMFMarkerNotRequired(t *testing.T) {
	m, _ := newSession(t, baseConfig(), nil)
	injectAt(t, m, eventPkt(t, 1, 1000, 101, 5, true, 160, true))
	ev, ok := awaitDTMF(t, m, time.Second)
	if !ok || ev.Event != 5 {
		t.Fatalf("marker %+v ok %v", ev, ok)
	}
	injectAt(t, m, eventPkt(t, 2, 2000, 101, 7, true, 160, false))
	ev, ok = awaitDTMF(t, m, time.Second)
	if !ok || ev.Event != 7 {
		t.Fatalf("no marker %+v ok %v", ev, ok)
	}
}

func TestReadDTMFKeepsADAndFlash(t *testing.T) {
	m, _ := newSession(t, baseConfig(), func(port int) RemoteMedia {
		r := pcmuMedia("127.0.0.1", port, port+1)
		r.DTMF.FMTP = "0-16"
		return r
	})
	injectAt(t, m, eventPkt(t, 1, 1000, 101, 12, true, 160, false))
	injectAt(t, m, eventPkt(t, 2, 2000, 101, 16, true, 160, false))
	a, ok := awaitDTMF(t, m, time.Second)
	flash, ok2 := awaitDTMF(t, m, time.Second)
	if !ok || !ok2 || a.Event != 12 || flash.Event != 16 {
		t.Fatalf("A %+v flash %+v", a, flash)
	}
}

func TestEventOutsideFMTPIsDropped(t *testing.T) {
	cfg := baseConfig()
	cfg.Jitter.Target = 20 * time.Millisecond
	m, _ := newSession(t, cfg, nil)
	a := bytes.Repeat([]byte{0x11}, frameSamples)
	b := bytes.Repeat([]byte{0x22}, frameSamples)
	injectAt(t, m, mustPkt(t, 1, 160, 42, 0, a))
	injectAt(t, m, eventPkt(t, 2, 320, 101, 16, true, 160, false))
	injectAt(t, m, mustPkt(t, 3, 480, 42, 0, b))
	if m.tones.pending() != 0 {
		t.Fatal("flash was emitted outside 0-15")
	}
	first := g711.DecodeULaw(a)
	second := g711.DecodeULaw(b)
	f1 := awaitPCM(t, m, time.Second)
	mid := awaitPCM(t, m, time.Second)
	f3 := awaitPCM(t, m, time.Second)
	if len(f1) != frameBytes || !bytes.Equal(f1, first) || !bytes.Equal(mid, first) || !bytes.Equal(f3, second) {
		t.Fatal("out-of-range event was not concealed as 20 ms")
	}
	if m.play.underrunCount() != 0 {
		t.Fatalf("underruns %d", m.play.underrunCount())
	}
}

func TestTelephoneEventDemuxConceals(t *testing.T) {
	cfg := baseConfig()
	cfg.Jitter.Target = 20 * time.Millisecond
	m, _ := newSession(t, cfg, nil)
	a := bytes.Repeat([]byte{0x11}, frameSamples)
	b := bytes.Repeat([]byte{0x22}, frameSamples)
	injectAt(t, m, mustPkt(t, 1, 160, 42, 0, a))
	injectAt(t, m, eventPkt(t, 2, 320, 101, 5, true, 480, false))
	injectAt(t, m, mustPkt(t, 3, 480, 42, 0, b))
	ev, ok := awaitDTMF(t, m, time.Second)
	if !ok || ev.Event != 5 {
		t.Fatalf("digit %+v ok %v", ev, ok)
	}
	first := g711.DecodeULaw(a)
	second := g711.DecodeULaw(b)
	f1 := awaitPCM(t, m, time.Second)
	mid := awaitPCM(t, m, time.Second)
	f3 := awaitPCM(t, m, time.Second)
	if len(f1) != frameBytes || !bytes.Equal(f1, first) || !bytes.Equal(mid, first) || !bytes.Equal(f3, second) {
		t.Fatal("telephone-event was not demuxed before G.711")
	}
	if m.play.underrunCount() != 0 {
		t.Fatalf("underruns %d", m.play.underrunCount())
	}
	loss, lok := m.play.loss()
	if !lok || loss.total != 0 {
		t.Fatalf("loss %+v ok %v", loss, lok)
	}
}

func TestWriteDTMFOnTheWire(t *testing.T) {
	m, sock := newSession(t, baseConfig(), func(port int) RemoteMedia {
		r := pcmuMedia("127.0.0.1", port, port+1)
		r.DTMF.Payload = 110
		return r
	})
	// 30 ms rounds up to 40 ms: one packet at 160, then 320, then three ends at 320.
	if err := m.WriteDTMF(DTMF{Event: 5, Duration: 30 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	var saw160, ended bool
	var ts uint32
	var haveTS bool
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && (!saw160 || !ended) {
		raw, _ := readUDP(t, sock, 200*time.Millisecond)
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		if pkt.PayloadType != 110 {
			continue
		}
		if len(pkt.Payload) != 4 || pkt.Payload[0] != 5 || pkt.Payload[1]&0x3f != dtmfVolume {
			t.Fatalf("payload %x", pkt.Payload)
		}
		if !haveTS {
			ts = pkt.Timestamp
			haveTS = true
		} else if pkt.Timestamp != ts {
			t.Fatalf("timestamp %d then %d", ts, pkt.Timestamp)
		}
		dur := binary.BigEndian.Uint16(pkt.Payload[2:])
		if pkt.Payload[1]&0x80 == 0 && dur == frameSamples {
			saw160 = true
		}
		if pkt.Payload[1]&0x80 != 0 && dur == 2*frameSamples {
			ended = true
		}
	}
	if !saw160 || !ended {
		t.Fatalf("saw160 %v ended %v", saw160, ended)
	}
}

func TestWriteDTMFNotNegotiatedSendsNothing(t *testing.T) {
	m, sock := newSession(t, baseConfig(), func(port int) RemoteMedia {
		r := pcmuMedia("127.0.0.1", port, port+1)
		r.DTMF = nil
		return r
	})
	err := m.WriteDTMF(DTMF{Event: 5})
	if !errors.Is(err, ErrDTMFNotNegotiated) {
		t.Fatalf("err %v", err)
	}
	for i := 0; i < 3; i++ {
		raw, _ := readUDP(t, sock, time.Second)
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		if pkt.PayloadType != 0 {
			t.Fatalf("sent payload type %d", pkt.PayloadType)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteDTMF(DTMF{Event: 1}); !errors.Is(err, ErrMediaClosed) {
		t.Fatalf("closed err %v", err)
	}
	if _, err := m.ReadDTMF(); !errors.Is(err, ErrMediaClosed) {
		t.Fatalf("read err %v", err)
	}
}

func TestReadDTMFUnblocksOnClose(t *testing.T) {
	m, _ := newSession(t, baseConfig(), nil)
	done := make(chan error, 1)
	go func() {
		_, err := m.ReadDTMF()
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrMediaClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadDTMF did not return")
	}
}

func TestWriteDTMFUsesDigitDuration(t *testing.T) {
	cfg := baseConfig()
	cfg.DTMF.DigitDuration = 40 * time.Millisecond
	m, sock := newSession(t, cfg, nil)
	if err := m.WriteDTMF(DTMF{Event: 3}); err != nil {
		t.Fatal(err)
	}
	var durs []uint16
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(durs) < 5 {
		raw, _ := readUDP(t, sock, 200*time.Millisecond)
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		if pkt.PayloadType != 101 || pkt.Payload[0] != 3 {
			continue
		}
		durs = append(durs, binary.BigEndian.Uint16(pkt.Payload[2:]))
	}
	if len(durs) != 5 || durs[0] != 160 || durs[1] != 320 {
		t.Fatalf("durations %v", durs)
	}
	for _, d := range durs[1:] {
		if d != 320 {
			t.Fatalf("durations %v", durs)
		}
	}
}
