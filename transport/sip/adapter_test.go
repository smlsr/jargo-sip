package sip

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/gojargo/jargo/frames"
	"github.com/gojargo/jargo/pipeline"
	"github.com/gojargo/jargo/transport"
	"github.com/gojargo/jargo/utils/events"
	"github.com/pion/rtp"
)

func audioParams(inRate, outRate int) transport.Params {
	p := transport.DefaultParams()
	p.AudioInSampleRate = inRate
	p.AudioOutSampleRate = outRate
	p.AudioInChannels = 2
	p.AudioOutChannels = 2
	p.AudioOut10msChunks = 9
	p.AudioOutEndSilenceSecs = 0
	return p
}

func testCall(t *testing.T, m *Media) *Call {
	t.Helper()
	c := &Call{
		done: make(chan struct{}),
		dlg:  &dialog{media: m, wake: make(chan struct{})},
	}
	t.Cleanup(func() {
		select {
		case <-c.done:
		default:
			c.finish()
		}
	})
	return c
}

func TestTransport8000Copies(t *testing.T) {
	call := testCall(t, nil)
	tr := NewTransport(call, audioParams(0, 0))
	if tr.pipe == nil || tr.pipe.Resamplers() != 0 {
		t.Fatalf("8000 built %d resamplers", tr.pipe.Resamplers())
	}
	if tr.in.Params().AudioInSampleRate != wireClock || tr.out.Params().AudioOutSampleRate != wireClock {
		t.Fatalf("rates in %d out %d", tr.in.Params().AudioInSampleRate, tr.out.Params().AudioOutSampleRate)
	}
	if tr.in.Params().AudioInChannels != 1 || tr.out.Params().AudioOutChannels != 1 {
		t.Fatalf("channels in %d out %d", tr.in.Params().AudioInChannels, tr.out.Params().AudioOutChannels)
	}
	if tr.out.Params().AudioOut10msChunks != 2 {
		t.Fatalf("chunks %d", tr.out.Params().AudioOut10msChunks)
	}
	if tr.out.Params().AudioOutAutoSilence != nil {
		t.Fatal("silence fill was turned off")
	}
	in := []byte{1, 2, 3, 4, 5, 6}
	out := tr.pipe.ToPipeline(in)
	if !bytes.Equal(in, out) {
		t.Fatalf("to pipeline %v", out)
	}
	out[0] ^= 0xFF
	if in[0] == out[0] {
		t.Fatal("8000 aliased the caller buffer")
	}
	back := tr.pipe.FromPipeline(in)
	if !bytes.Equal(in, back) {
		t.Fatalf("from pipeline %v", back)
	}
	if !tr.out.SupportsNativeDTMF() {
		t.Fatal("native DTMF is off")
	}
	var iface transport.Transport = tr
	if iface.Input() == nil || iface.Output() == nil || iface.Events() == nil {
		t.Fatal("transport shape")
	}
}

func TestTransportResamplesOnce(t *testing.T) {
	call := testCall(t, nil)
	up := NewTransport(call, audioParams(16000, wireClock))
	if up.pipe.Resamplers() != 1 || up.pipe.in == nil || up.pipe.out != nil {
		t.Fatalf("16 kHz input built resamplers=%d in=%v out=%v", up.pipe.Resamplers(), up.pipe.in != nil, up.pipe.out != nil)
	}
	wide := up.pipe.ToPipeline(bytes.Repeat([]byte{0, 0}, frameSamples))
	if len(wide) != frameBytes*2 {
		t.Fatalf("8000 to 16000 produced %d bytes", len(wide))
	}

	both := NewTransport(testCall(t, nil), audioParams(16000, 16000))
	if both.pipe.Resamplers() != 2 || both.pipe.in == nil || both.pipe.out == nil {
		t.Fatalf("both directions built %d", both.pipe.Resamplers())
	}
	narrow := both.pipe.FromPipeline(bytes.Repeat([]byte{1, 0}, frameSamples*2))
	if len(narrow) != frameBytes {
		t.Fatalf("16000 to 8000 produced %d bytes", len(narrow))
	}
	// A second conversion does not build another stream.
	if both.pipe.Resamplers() != 2 {
		t.Fatalf("resamplers changed to %d", both.pipe.Resamplers())
	}
}

func TestKeypadZeroThroughEleven(t *testing.T) {
	m, _ := newSession(t, baseConfig(), nil)
	m.tones.setSpec(true, "0-16")
	call := testCall(t, m)
	tr := NewTransport(call, audioParams(wireClock, wireClock))
	audio := startInput(t, tr)

	injectAt(t, m, eventPkt(t, 40, 4000, 101, 12, true, 160, false))
	injectAt(t, m, eventPkt(t, 41, 5000, 101, 16, true, 160, false))
	ev, ok := awaitDTMF(t, m, time.Second)
	if !ok || ev.Event != 12 {
		t.Fatalf("ReadDTMF A %v %v", ev, ok)
	}
	ev, ok = awaitDTMF(t, m, time.Second)
	if !ok || ev.Event != 16 {
		t.Fatalf("ReadDTMF flash %v %v", ev, ok)
	}
	select {
	case key := <-audio.keys:
		t.Fatalf("non-keypad frame %s", key)
	case <-time.After(200 * time.Millisecond):
	}

	want := []frames.KeypadEntry{
		frames.KeypadZero, frames.KeypadOne, frames.KeypadTwo, frames.KeypadThree,
		frames.KeypadFour, frames.KeypadFive, frames.KeypadSix, frames.KeypadSeven,
		frames.KeypadEight, frames.KeypadNine, frames.KeypadStar, frames.KeypadPound,
	}
	for i, event := range []uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11} {
		injectAt(t, m, eventPkt(t, uint16(10+i), uint32(i+1)*160, 101, event, true, 160, false))
		got := awaitKey(t, audio)
		if got != want[i] {
			t.Fatalf("event %d frame %s want %s", event, got, want[i])
		}
	}
}

func TestEndFrameHangsUpAfterAudio(t *testing.T) {
	a := startAudio(t, baseConfig())
	call := testCall(t, a.m)
	call.dlg.pair = a.m.pair
	tr := NewTransport(call, audioParams(wireClock, wireClock))
	left := disconnects(tr)
	task := startOutput(t, tr)

	const n = 8
	pcm := bytes.Repeat([]byte{0x10, 0x20}, frameSamples)
	for range n {
		task.QueueFrame(frames.NewOutputAudioRawFrame(pcm, wireClock, 1))
	}
	task.QueueFrame(frames.NewEndFrame())
	want, err := encodePCM(CodecPCMU, pcm)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPayload(t, a.sink, want, n, 2*time.Second); got != n {
		t.Fatalf("RTP carried %d of %d farewell frames", got, n)
	}
	waitDone(t, call, 2*time.Second)
	a.m.send.mu.Lock()
	queued := a.m.send.pcmLen()
	a.m.send.mu.Unlock()
	if queued != 0 {
		t.Fatalf("%d farewell bytes were still queued after BYE", queued)
	}
	if tr.call != call {
		t.Fatal("transport swapped the call")
	}
	assertQuiet(t, left)
}

func TestCancelFrameHangsUp(t *testing.T) {
	a := startAudio(t, baseConfig())
	call := testCall(t, a.m)
	call.dlg.pair = a.m.pair
	tr := NewTransport(call, audioParams(wireClock, wireClock))
	left := disconnects(tr)
	task := startOutput(t, tr)

	pcm := bytes.Repeat([]byte{0x10, 0x20}, maxPCMQueue/2)
	if err := call.WritePCM(pcm); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	task.QueueFrame(frames.NewCancelFrame())
	waitDone(t, call, time.Second)
	if time.Since(started) > time.Second {
		t.Fatalf("cancel waited %s", time.Since(started))
	}
	a.m.send.mu.Lock()
	queued := a.m.send.pcmLen()
	a.m.send.mu.Unlock()
	if queued == 0 {
		t.Fatal("CancelFrame transmitted the queued audio")
	}
	if tr.call != call {
		t.Fatal("transport swapped the call")
	}
	assertQuiet(t, left)
}

func TestAdapterUrgentDTMFCutsAheadOfPCM(t *testing.T) {
	a := startAudio(t, baseConfig())
	call := testCall(t, a.m)
	tr := NewTransport(call, audioParams(wireClock, wireClock))
	queued := bytes.Repeat([]byte{0x10, 0x20}, 2240)
	if err := call.WritePCM(queued); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := tr.out.WriteDTMFNative(ctx, frames.NewOutputDTMFFrame(frames.KeypadFive)); err != nil {
		t.Fatal(err)
	}
	ev, before := dtmfPlace(a.m.send, 5)
	if !ev || before == 0 {
		t.Fatalf("normal digit ev=%v pcm before=%d", ev, before)
	}
	if err := tr.out.WriteDTMFNative(ctx, frames.NewOutputDTMFUrgentFrame(frames.KeypadSix)); err != nil {
		t.Fatal(err)
	}
	ev, before = dtmfPlace(a.m.send, 6)
	if !ev || before != 0 {
		t.Fatalf("urgent digit ev=%v pcm before=%d", ev, before)
	}
}

func TestEarlyMediaFramesFirst(t *testing.T) {
	a := startAudio(t, baseConfig())
	for seq := uint16(1); seq <= 4; seq++ {
		a.send(t, seq, ulawTone(int16(seq*20)))
	}
	waitFor(t, 2*time.Second, func() bool { return a.m.play.queued() >= 2 })
	a.m.play.mu.Lock()
	first := append([]byte(nil), a.m.play.out[0]...)
	second := append([]byte(nil), a.m.play.out[1]...)
	a.m.play.mu.Unlock()

	call := testCall(t, a.m)
	tr := NewTransport(call, audioParams(wireClock, wireClock))
	if tr.call != call {
		t.Fatal("transport swapped the call")
	}
	got := startInput(t, tr)
	if !bytes.Equal(awaitPCMFrame(t, got), first) {
		t.Fatal("first frame was not the early-media frame already queued")
	}
	if !bytes.Equal(awaitPCMFrame(t, got), second) {
		t.Fatal("second frame was not the next early-media frame")
	}
}

func TestRemoteByeDisconnectsAfterReplaced(t *testing.T) {
	ep := startEP(t, baseConfig())
	m, _ := newSession(t, baseConfig(), nil)
	call := testCall(t, m)
	fresh := testCall(t, nil)
	tr := NewTransport(call, audioParams(wireClock, wireClock))
	left := disconnects(tr)

	replaced := make(chan struct{})
	ep.OnReplaced(func(old, new *Call) {
		if old != call || new != fresh {
			t.Errorf("replaced %p %p", old, new)
		}
		select {
		case <-old.Done():
			t.Error("old call ended inside OnReplaced")
		default:
		}
		close(replaced)
	})
	ep.fireReplaced(call, fresh)
	select {
	case <-replaced:
	case <-time.After(time.Second):
		t.Fatal("OnReplaced did not run")
	}
	select {
	case <-left:
		t.Fatal("disconnect ran before the call ended")
	default:
	}
	call.finish()
	select {
	case got := <-left:
		if got != call {
			t.Fatalf("disconnect client %p", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote end did not disconnect")
	}
	if tr.call != call {
		t.Fatal("transport swapped the call")
	}
}

type inputFrames struct {
	pcm  chan []byte
	keys chan frames.KeypadEntry
}

func startInput(t *testing.T, tr *Transport) *inputFrames {
	t.Helper()
	got := &inputFrames{
		pcm:  make(chan []byte, 32),
		keys: make(chan frames.KeypadEntry, 16),
	}
	task := pipeline.NewWorker(pipeline.New(tr.Input()), pipeline.WorkerConfig{
		ReachedDownstreamFilter: pipeline.AnyFrame,
	})
	events.On(&task.Registry, pipeline.EventFrameReachedDownstream, func(_ context.Context, f frames.Frame) {
		switch fr := f.(type) {
		case *frames.InputAudioRawFrame:
			got.pcm <- append([]byte(nil), fr.Audio...)
		case *frames.InputDTMFFrame:
			got.keys <- fr.Button
		}
	})
	done := make(chan struct{})
	go func() {
		_ = task.Run(context.Background())
		close(done)
	}()
	t.Cleanup(func() {
		task.StopWhenDone()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	return got
}

func startOutput(t *testing.T, tr *Transport) *pipeline.Worker {
	t.Helper()
	task := pipeline.NewWorker(pipeline.New(tr.Output()), pipeline.WorkerConfig{})
	done := make(chan struct{})
	go func() {
		_ = task.Run(context.Background())
		close(done)
	}()
	t.Cleanup(func() {
		task.StopWhenDone()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := task.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	return task
}

func disconnects(tr *Transport) <-chan *Call {
	ch := make(chan *Call, 1)
	events.On(tr.Events(), transport.EventClientDisconnected, func(_ context.Context, c *Call) {
		ch <- c
	})
	return ch
}

func awaitKey(t *testing.T, got *inputFrames) frames.KeypadEntry {
	t.Helper()
	select {
	case key := <-got.keys:
		return key
	case <-time.After(2 * time.Second):
		t.Fatal("keypad frame was not emitted")
	}
	return ""
}

func awaitPCMFrame(t *testing.T, got *inputFrames) []byte {
	t.Helper()
	select {
	case pcm := <-got.pcm:
		return pcm
	case <-time.After(2 * time.Second):
		t.Fatal("audio frame was not emitted")
	}
	return nil
}

func countPayload(t *testing.T, sink *net.UDPConn, want []byte, n int, d time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(d)
	got := 0
	for got < n && time.Now().Before(deadline) {
		remain := time.Until(deadline)
		if remain > 200*time.Millisecond {
			remain = 200 * time.Millisecond
		}
		buf := make([]byte, 2048)
		_ = sink.SetReadDeadline(time.Now().Add(remain))
		nn, _, err := sink.ReadFromUDP(buf)
		if err != nil || nn == 0 {
			continue
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:nn]); err != nil {
			continue
		}
		if bytes.Equal(pkt.Payload, want) {
			got++
		}
	}
	return got
}

func dtmfPlace(s *sender, event uint8) (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := 0
	for _, it := range s.items {
		if it.ev != nil && it.ev.event == event {
			return true, before
		}
		if it.ev == nil {
			before += len(it.pcm)
		}
	}
	return false, before
}

func waitDone(t *testing.T, c *Call, d time.Duration) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(d):
		t.Fatal("call stayed up")
	}
}

func assertQuiet(t *testing.T, left <-chan *Call) {
	t.Helper()
	select {
	case <-left:
		t.Fatal("our hangup was reported as the client leaving")
	case <-time.After(200 * time.Millisecond):
	}
}
