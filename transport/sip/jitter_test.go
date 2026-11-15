package sip

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/gojargo/jargo/audio/g711"
	"github.com/gojargo/jargo/audio/resample"
	"github.com/pion/rtp"
)

func testJitter(target, min, max time.Duration) *jitter {
	return newJitter(JitterConfig{Target: target, Min: min, Max: max}, CodecPCMU)
}

func pcmTone(seed byte) []byte {
	p := make([]byte, frameBytes)
	for i := range p {
		p[i] = seed + byte(i)
	}
	return p
}

func TestTarget60DoesNotStartEarly(t *testing.T) {
	j := testJitter(60*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond)
	now := time.Unix(1, 0)
	first := pcmTone(1)
	j.pushAudio(7, 10, first, now)
	if _, ok := j.step(now); ok {
		t.Fatal("playout started on 20 ms of audio")
	}
	now = now.Add(tickInterval)
	j.pushAudio(7, 11, pcmTone(2), now)
	if _, ok := j.step(now); ok {
		t.Fatal("playout started on 40 ms of audio")
	}
	now = now.Add(tickInterval)
	j.pushAudio(7, 12, pcmTone(3), now)
	frame, ok := j.step(now)
	if !ok {
		t.Fatal("playout did not start once 60 ms was buffered")
	}
	if !bytes.Equal(frame, first) {
		t.Fatal("the first frame was not the oldest packet")
	}
	if j.underrunCount() != 0 {
		t.Fatalf("underruns %d", j.underrunCount())
	}
}

func TestOldestAgeStartsAfterTarget(t *testing.T) {
	j := testJitter(60*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond)
	now := time.Unix(1, 0)
	tone := pcmTone(4)
	j.pushAudio(7, 1, tone, now)
	if _, ok := j.step(now.Add(60 * time.Millisecond)); ok {
		t.Fatal("age equal to target started playout")
	}
	frame, ok := j.step(now.Add(60*time.Millisecond + time.Nanosecond))
	if !ok {
		t.Fatal("playout did not start once the oldest packet was older than target")
	}
	if !bytes.Equal(frame, tone) {
		t.Fatal("played a different frame")
	}
}

func TestDroppedPacketIsOneConcealment(t *testing.T) {
	j := testJitter(40*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond)
	now := time.Unix(1, 0)
	a := pcmTone(10)
	b := pcmTone(20)
	c := pcmTone(30)
	j.pushAudio(7, 1, a, now)
	j.pushAudio(7, 2, b, now)
	j.pushAudio(7, 4, c, now)
	f1, ok := j.step(now)
	f2, ok2 := j.step(now.Add(tickInterval))
	conceal, ok3 := j.step(now.Add(2 * tickInterval))
	f4, ok4 := j.step(now.Add(3 * tickInterval))
	if !ok || !ok2 || !ok3 || !ok4 {
		t.Fatal("a tick did not produce a frame")
	}
	if !bytes.Equal(f1, a) || !bytes.Equal(f2, b) || !bytes.Equal(f4, c) {
		t.Fatal("playout order changed")
	}
	if !bytes.Equal(conceal, b) {
		t.Fatal("the gap was not one repeat of the previous frame")
	}
	if j.underrunCount() != 1 {
		t.Fatalf("underruns %d", j.underrunCount())
	}
	if j.delay() != 60*time.Millisecond {
		t.Fatalf("target after underrun %s", j.delay())
	}
}

func TestLatePacketDoesNotRewind(t *testing.T) {
	j := testJitter(40*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond)
	now := time.Unix(1, 0)
	j.pushAudio(7, 1, pcmTone(1), now)
	j.pushAudio(7, 2, pcmTone(2), now)
	j.pushAudio(7, 3, pcmTone(3), now)
	if _, ok := j.step(now); !ok {
		t.Fatal("did not start")
	}
	j.pushAudio(7, 1, pcmTone(9), now.Add(tickInterval))
	frame, ok := j.step(now.Add(tickInterval))
	if !ok || !bytes.Equal(frame, pcmTone(2)) {
		t.Fatal("a late packet moved the playout head")
	}
}

func TestHighDepthLowersTarget(t *testing.T) {
	j := testJitter(40*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond)
	now := time.Unix(1, 0)
	tone := pcmTone(5)
	for seq := uint16(0); seq < 8; seq++ {
		j.pushAudio(7, seq, tone, now)
	}
	for n := 0; n < 150; n++ {
		j.pushAudio(7, uint16(8+n), tone, now)
		if _, ok := j.step(now); !ok {
			t.Fatalf("tick %d produced no frame", n)
		}
		if j.underrunCount() != 0 {
			t.Fatalf("underrun while the buffer was full at %s", now)
		}
		if j.delay() < 40*time.Millisecond {
			if now.Sub(time.Unix(1, 0)) < adaptEvery {
				t.Fatalf("target fell to %s after %s", j.delay(), now.Sub(time.Unix(1, 0)))
			}
			if j.delay() != 20*time.Millisecond {
				t.Fatalf("target %s", j.delay())
			}
			return
		}
		now = now.Add(tickInterval)
	}
	t.Fatalf("target stayed %s", j.delay())
}

func TestResetDropsBufferedAudio(t *testing.T) {
	j := testJitter(40*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond)
	now := time.Unix(1, 0)
	j.pushAudio(7, 1, pcmTone(1), now)
	j.pushAudio(7, 2, pcmTone(2), now)
	if !j.reset() {
		t.Fatal("reset reported an empty buffer")
	}
	if _, ok := j.step(now.Add(time.Second)); ok {
		t.Fatal("playout continued after reset")
	}
	if j.packets() != 0 {
		t.Fatalf("loss state survived reset: %d", j.packets())
	}
}

func TestTelephoneEventConcealsWithoutUnderrun(t *testing.T) {
	j := testJitter(20*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond)
	now := time.Unix(1, 0)
	a := pcmTone(1)
	b := pcmTone(2)
	j.pushAudio(7, 1, a, now)
	j.pushEvent(7, 2, now)
	j.pushAudio(7, 3, b, now)
	f1, ok1 := j.step(now)
	conceal, ok2 := j.step(now)
	f3, ok3 := j.step(now)
	if !ok1 || !ok2 || !ok3 {
		t.Fatal("event span did not play")
	}
	if !bytes.Equal(f1, a) || !bytes.Equal(conceal, a) || !bytes.Equal(f3, b) {
		t.Fatal("telephone-event was not concealed as 20 ms of the previous frame")
	}
	if j.underrunCount() != 0 {
		t.Fatalf("event counted as underrun %d", j.underrunCount())
	}
	loss, ok := j.loss()
	if !ok || loss.total != 0 || loss.ssrc != 7 {
		t.Fatalf("loss %+v ok %v", loss, ok)
	}
}

func TestTenMillisecondFramesCombine(t *testing.T) {
	j := testJitter(20*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond)
	now := time.Unix(1, 0)
	a := bytes.Repeat([]byte{1, 2}, 80)
	b := bytes.Repeat([]byte{3, 4}, 80)
	j.pushAudio(7, 1, a, now)
	j.pushAudio(7, 2, b, now)
	frame, ok := j.step(now)
	if !ok {
		t.Fatal("10 ms pair did not play")
	}
	if len(frame) != frameBytes || !bytes.Equal(frame[:len(a)], a) || !bytes.Equal(frame[len(a):], b) {
		t.Fatal("10 ms frames were not one 20 ms playout")
	}
	if j.underrunCount() != 0 {
		t.Fatalf("underruns %d", j.underrunCount())
	}
}

func TestLossCountsGapNotEvent(t *testing.T) {
	j := testJitter(40*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond)
	now := time.Unix(1, 0)
	j.pushAudio(9, 1, pcmTone(1), now)
	j.pushAudio(9, 3, pcmTone(3), now)
	loss, ok := j.loss()
	if !ok || loss.total != 1 || loss.ext != 3 {
		t.Fatalf("gap loss %+v ok %v", loss, ok)
	}
	j.pushEvent(9, 4, now)
	loss, ok = j.loss()
	if !ok || loss.total != 1 {
		t.Fatalf("event changed loss %+v", loss)
	}
}

func TestJitterRoundsToFrame(t *testing.T) {
	cfg := baseConfig()
	cfg.Jitter.Target = 25 * time.Millisecond
	cfg.Jitter.Min = 21 * time.Millisecond
	cfg.Jitter.Max = 50 * time.Millisecond
	p, err := prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.Jitter.Target != 40*time.Millisecond || p.cfg.Jitter.Min != 40*time.Millisecond || p.cfg.Jitter.Max != 60*time.Millisecond {
		t.Fatalf("round %+v", p.cfg.Jitter)
	}

	cfg.Jitter = JitterConfig{Target: 100 * time.Millisecond, Min: 20 * time.Millisecond, Max: 50 * time.Millisecond}
	p, err = prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.Jitter.Target != 60*time.Millisecond || p.cfg.Jitter.Max != 60*time.Millisecond || p.cfg.Jitter.Min != 20*time.Millisecond {
		t.Fatalf("clamp %+v", p.cfg.Jitter)
	}

	cfg.Jitter = JitterConfig{Target: 40 * time.Millisecond, Min: 80 * time.Millisecond, Max: 200 * time.Millisecond}
	p, err = prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.Jitter.Min != 40*time.Millisecond || p.cfg.Jitter.Target != 40*time.Millisecond {
		t.Fatalf("min %+v", p.cfg.Jitter)
	}
}

type audioFix struct {
	m    *Media
	peer *net.UDPConn
	dst  *net.UDPAddr
	sink *net.UDPConn
}

func startAudio(t *testing.T, cfg Config) audioFix {
	t.Helper()
	ep := startEP(t, cfg)
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sink := listenLoop(t)
	sinkAddr := sink.LocalAddr().(*net.UDPAddr)
	m, err := StartMedia(context.Background(), pair, pcmuMedia("127.0.0.1", sinkAddr.Port, sinkAddr.Port+1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	peer := listenLoop(t)
	return audioFix{
		m:    m,
		peer: peer,
		dst:  &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: pair.LocalRTPPort},
		sink: sink,
	}
}

func (a audioFix) send(t *testing.T, seq uint16, payload []byte) {
	t.Helper()
	raw := mustPkt(t, seq, uint32(seq)*uint32(frameSamples), 77, 0, payload)
	if _, err := a.peer.WriteToUDP(raw, a.dst); err != nil {
		t.Fatal(err)
	}
}

func ulawTone(seed int16) []byte {
	pcm := make([]byte, frameBytes)
	for i := 0; i < len(pcm); i += 2 {
		s := seed + int16(i)
		pcm[i] = byte(s)
		pcm[i+1] = byte(uint16(s) >> 8)
	}
	return g711.EncodeULaw(pcm)
}

func readPCMWithin(t *testing.T, m *Media, d time.Duration) ([]byte, error) {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := m.ReadPCM()
		ch <- result{b, err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-time.After(d):
		return nil, errors.New("timeout")
	}
}

func TestReadPCMWaitsFor60ms(t *testing.T) {
	cfg := baseConfig()
	cfg.Jitter.Target = 60 * time.Millisecond
	a := startAudio(t, cfg)
	early := make(chan []byte, 1)
	go func() {
		b, err := a.m.ReadPCM()
		if err == nil {
			early <- b
		}
	}()
	a.send(t, 1, ulawTone(100))
	a.send(t, 2, ulawTone(200))
	waitFor(t, time.Second, func() bool { return a.m.play.packets() >= 2 })
	select {
	case <-early:
		t.Fatal("playout started before 60 ms of audio")
	case <-time.After(25 * time.Millisecond):
	}
	if a.m.play.running() {
		t.Fatal("playout was running on 40 ms of audio")
	}
	a.send(t, 3, ulawTone(300))
	select {
	case frame := <-early:
		if len(frame) != frameBytes {
			t.Fatalf("frame %d", len(frame))
		}
	case <-time.After(time.Second):
		t.Fatal("playout did not start")
	}
}

func TestReadPCMConcealmentFrame(t *testing.T) {
	cfg := baseConfig()
	cfg.Jitter.Target = 200 * time.Millisecond
	a := startAudio(t, cfg)
	a.send(t, 1, ulawTone(1000))
	a.send(t, 2, ulawTone(2000))
	a.send(t, 3, ulawTone(3000))
	a.send(t, 5, ulawTone(5000))
	waitFor(t, time.Second, func() bool { return a.m.play.packets() == 4 })
	if a.m.play.running() {
		t.Fatal("playout started before the gap was buffered")
	}
	var frames [][]byte
	for i := 0; i < 5; i++ {
		frame, err := readPCMWithin(t, a.m, 2*time.Second)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if len(frame) != frameBytes {
			t.Fatalf("frame %d len %d", i, len(frame))
		}
		frames = append(frames, frame)
	}
	if bytes.Equal(frames[0], frames[1]) || bytes.Equal(frames[1], frames[2]) {
		t.Fatal("distinct packets decoded to the same frame")
	}
	if !bytes.Equal(frames[3], frames[2]) {
		t.Fatal("dropped packet was not one concealment frame")
	}
	if bytes.Equal(frames[4], frames[3]) {
		t.Fatal("audio after the gap repeated the concealment")
	}
}

func TestReadPCMClosed(t *testing.T) {
	a := startAudio(t, baseConfig())
	errc := make(chan error, 1)
	go func() {
		_, err := a.m.ReadPCM()
		errc <- err
	}()
	time.Sleep(15 * time.Millisecond)
	if err := a.m.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrMediaClosed) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadPCM stayed blocked after close")
	}
}

func TestRetargetClearsPipelineAndJitter(t *testing.T) {
	cfg := baseConfig()
	cfg.Jitter.Target = 200 * time.Millisecond
	a := startAudio(t, cfg)
	p, err := NewPipeline(16000, 16000)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.m.UsePipeline(p); err != nil {
		t.Fatal(err)
	}
	a.send(t, 1, ulawTone(8))
	waitFor(t, time.Second, func() bool { return a.m.play.packets() == 1 })
	before := p.clearCount()
	next := listenLoop(t)
	addr := next.LocalAddr().(*net.UDPAddr)
	if err := a.m.Retarget(pcmuMedia("127.0.0.1", addr.Port, addr.Port+1)); err != nil {
		t.Fatal(err)
	}
	if p.clearCount() != before+1 {
		t.Fatalf("clears %d -> %d", before, p.clearCount())
	}
	if a.m.play.packets() != 0 || a.m.play.running() {
		t.Fatal("jitter kept audio from the old address")
	}
	got := make(chan []byte, 1)
	go func() {
		b, err := a.m.ReadPCM()
		if err == nil {
			got <- b
		}
	}()
	select {
	case <-got:
		t.Fatal("ReadPCM returned audio discarded with the old address")
	case <-time.After(250 * time.Millisecond):
	}
}

func TestPCM320BecomesOnePayload(t *testing.T) {
	a := startAudio(t, baseConfig())
	pcm := pcmTone(40)
	if len(pcm) != 320 {
		t.Fatalf("pcm %d", len(pcm))
	}
	if err := a.m.WritePCM(pcm); err != nil {
		t.Fatal(err)
	}
	want, err := encodePCM(CodecPCMU, pcm)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != frameSamples {
		t.Fatalf("payload %d", len(want))
	}
	matched := 0
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		raw, _ := readUDP(t, a.sink, 200*time.Millisecond)
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		if len(pkt.Payload) != frameSamples {
			t.Fatalf("payload %d", len(pkt.Payload))
		}
		if bytes.Equal(pkt.Payload, want) {
			matched++
		}
	}
	if matched != 1 {
		t.Fatalf("320 bytes became %d payloads", matched)
	}
}

func TestPipeline8000BuildsNoResampler(t *testing.T) {
	if resample.NeverClear >= 0 {
		t.Fatalf("NeverClear %s", resample.NeverClear)
	}
	p, err := NewPipeline(8000, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if p.Resamplers() != 0 {
		t.Fatalf("streams %d", p.Resamplers())
	}
	in := pcmTone(7)
	out := p.FromPipeline(in)
	if !bytes.Equal(in, out) {
		t.Fatal("8000 was not a copy")
	}
	out[0] ^= 0xFF
	if in[0] == out[0] {
		t.Fatal("8000 aliased the caller's buffer")
	}
	zero, err := NewPipeline(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if zero.Resamplers() != 0 {
		t.Fatalf("zero rate built %d streams", zero.Resamplers())
	}
}

func TestPipeline16000PayloadStays160(t *testing.T) {
	outOnly, err := NewPipeline(8000, 16000)
	if err != nil {
		t.Fatal(err)
	}
	if outOnly.Resamplers() != 1 {
		t.Fatalf("16000 output built %d resamplers", outOnly.Resamplers())
	}
	both, err := NewPipeline(16000, 16000)
	if err != nil {
		t.Fatal(err)
	}
	if both.Resamplers() != 2 {
		t.Fatalf("both directions built %d resamplers", both.Resamplers())
	}
	pcm16 := make([]byte, 640)
	for i := 0; i < len(pcm16); i += 2 {
		s := int16(i - 200)
		pcm16[i] = byte(s)
		pcm16[i+1] = byte(uint16(s) >> 8)
	}
	wire := outOnly.FromPipeline(pcm16)
	if len(wire) != frameBytes {
		t.Fatalf("16 kHz frame became %d bytes at 8000", len(wire))
	}
	up := both.ToPipeline(wire)
	if len(up) != 640 {
		t.Fatalf("8000 to 16000 produced %d bytes", len(up))
	}
	down, err := NewPipeline(4000, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if down.Resamplers() != 1 {
		t.Fatalf("4000 built %d resamplers", down.Resamplers())
	}
	if got := down.ToPipeline(wire); len(got) != frameBytes/2 {
		t.Fatalf("downsample produced %d bytes", len(got))
	}

	a := startAudio(t, baseConfig())
	if err := a.m.WritePCM(wire); err != nil {
		t.Fatal(err)
	}
	want, err := encodePCM(CodecPCMU, wire)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		raw, _ := readUDP(t, a.sink, 200*time.Millisecond)
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		if len(pkt.Payload) != frameSamples {
			t.Fatalf("payload %d", len(pkt.Payload))
		}
		if bytes.Equal(pkt.Payload, want) {
			return
		}
	}
	t.Fatal("16000 pipeline did not become one 160-byte payload")
}
