package sip

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/gojargo/jargo/frames"
	"github.com/gojargo/jargo/processor"
	"github.com/gojargo/jargo/transport"
	"github.com/gojargo/jargo/utils/events"
)

// Transport adapts one Call to a jargo pipeline. It matches transport/rtc:
// Input, Output, and Events, with the Call in the client-connected and
// client-disconnected events. The Call pointer given to NewTransport stays
// for the life of the transport.
type Transport struct {
	in   *inputTransport
	out  *outputTransport
	call *Call
	pipe *Pipeline
	// closing is set before our own Hangup, so that Bye is not reported as
	// the client leaving. A remote BYE, MaxCall, or Endpoint.Close leaves it
	// clear and the watch raises EventClientDisconnected.
	closing atomic.Bool
}

// NewTransport builds the pipeline adapter for c. A zero sample rate becomes
// 8000. Channels are forced to 1. AudioOut10msChunks is 2 so the base hands
// over 20 ms, one G.711 frame. AudioOutAutoSilence is left as the caller set
// it, and nil keeps silence fill on.
func NewTransport(c *Call, p transport.Params) *Transport {
	p.AudioOut10msChunks = 2
	p.AudioInChannels = 1
	p.AudioOutChannels = 1
	if p.AudioInSampleRate == 0 {
		p.AudioInSampleRate = wireClock
	}
	if p.AudioOutSampleRate == 0 {
		p.AudioOutSampleRate = wireClock
	}
	pipe, err := NewPipeline(p.AudioInSampleRate, p.AudioOutSampleRate)
	if err != nil {
		pipe = nil
	}
	if c != nil && c.dlg != nil && c.dlg.media != nil && pipe != nil {
		_ = c.dlg.media.UsePipeline(pipe)
	}

	t := &Transport{call: c, pipe: pipe}
	t.in = newInput(t, c, pipe, p)
	t.out = newOutput(c, pipe, p, &t.closing)

	ev := t.in.Events()
	ev.Register(transport.EventAppMessage, false)
	ev.Register(transport.EventClientConnected, false)
	ev.Register(transport.EventClientDisconnected, false)

	go t.watch()
	return t
}

// Input returns the processor that turns Call audio into pipeline frames.
func (t *Transport) Input() processor.Processor { return t.in }

// Output returns the processor that turns pipeline frames into Call audio.
func (t *Transport) Output() processor.Processor { return t.out }

// Events returns the registry of client and application events.
func (t *Transport) Events() *events.Registry { return t.in.Events() }

func (t *Transport) watch() {
	if t == nil || t.call == nil {
		return
	}
	done := t.call.Done()
	if done == nil {
		return
	}
	<-done
	if t.closing.Load() {
		return
	}
	ctx := context.Background()
	if c := t.in.readContext(); c != nil {
		ctx = context.WithoutCancel(c)
	}
	t.in.Events().Call(ctx, transport.EventClientDisconnected, t, t.call)
}

func (t *Transport) noteConnected(ctx context.Context) {
	if t == nil || t.call == nil {
		return
	}
	select {
	case <-t.call.Done():
		return
	default:
	}
	t.in.Events().Call(ctx, transport.EventClientConnected, t, t.call)
	t.in.PushClientConnected(ctx)
}

type inputTransport struct {
	*transport.BaseInput
	owner *Transport
	call  *Call
	pipe  *Pipeline

	readWG     sync.WaitGroup
	mu         sync.Mutex
	readCancel context.CancelFunc
	readCtx    context.Context
}

func newInput(owner *Transport, c *Call, pipe *Pipeline, p transport.Params) *inputTransport {
	in := &inputTransport{owner: owner, call: c, pipe: pipe}
	in.BaseInput = transport.NewBaseInput("SIPInput", p, in)
	return in
}

func (in *inputTransport) readContext() context.Context {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.readCtx
}

// StartReading plays Call audio into the pipeline. Frames already waiting on
// ReadPCM, including 183 early media, are emitted first because that is the
// order ReadPCM returns them.
func (in *inputTransport) StartReading(ctx context.Context) error {
	if in.call == nil {
		return nil
	}
	in.mu.Lock()
	if in.readCancel != nil {
		in.mu.Unlock()
		return nil
	}
	readCtx, cancel := context.WithCancel(ctx)
	in.readCancel = cancel
	in.readCtx = readCtx
	in.mu.Unlock()

	in.readWG.Add(2)
	go in.readAudio(readCtx)
	go in.readKeys(readCtx)
	in.owner.noteConnected(readCtx)
	return nil
}

// StopReading stops the play and keypad loops.
func (in *inputTransport) StopReading(context.Context) error {
	in.mu.Lock()
	cancel := in.readCancel
	in.readCancel = nil
	in.readCtx = nil
	in.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	in.readWG.Wait()
	return nil
}

func (in *inputTransport) sampleRate() int {
	if r := in.SampleRate(); r > 0 {
		return r
	}
	if r := in.Params().AudioInSampleRate; r > 0 {
		return r
	}
	return wireClock
}

func (in *inputTransport) readAudio(ctx context.Context) {
	defer in.readWG.Done()
	rate := in.sampleRate()
	for {
		pcm, err := nextPCM(ctx, in.call)
		if err != nil {
			return
		}
		if in.pipe != nil {
			pcm = in.pipe.ToPipeline(pcm)
		}
		if len(pcm) == 0 {
			continue
		}
		in.PushAudioFrame(ctx, frames.NewInputAudioRawFrame(pcm, rate, 1))
	}
}

func (in *inputTransport) readKeys(ctx context.Context) {
	defer in.readWG.Done()
	for {
		ev, err := nextKey(ctx, in.call)
		if err != nil {
			return
		}
		button, ok := buttonForEvent(ev.Event)
		if !ok {
			continue
		}
		// InputDTMFFrame is a system frame, so a barge-in does not drop it.
		_ = in.PushFrame(ctx, frames.NewInputDTMFFrame(button), processor.Downstream)
	}
}

func nextPCM(ctx context.Context, c *Call) ([]byte, error) {
	type result struct {
		pcm []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		pcm, err := c.ReadPCM()
		ch <- result{pcm, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.Done():
		return nil, ErrMediaClosed
	case r := <-ch:
		return r.pcm, r.err
	}
}

func nextKey(ctx context.Context, c *Call) (DTMF, error) {
	type result struct {
		ev  DTMF
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ev, err := c.readKeypad()
		ch <- result{ev, err}
	}()
	select {
	case <-ctx.Done():
		return DTMF{}, ctx.Err()
	case <-c.Done():
		return DTMF{}, ErrMediaClosed
	case r := <-ch:
		return r.ev, r.err
	}
}

type outputTransport struct {
	*transport.BaseOutput
	call    *Call
	pipe    *Pipeline
	closing *atomic.Bool
	once    sync.Once
}

func newOutput(c *Call, pipe *Pipeline, p transport.Params, closing *atomic.Bool) *outputTransport {
	out := &outputTransport{call: c, pipe: pipe, closing: closing}
	out.BaseOutput = transport.NewBaseOutput("SIPOutput", p, out)
	return out
}

// ProcessFrame lets the base drain queued audio on EndFrame, and skip that
// wait on CancelFrame, then sends BYE. closing is set first so the watch
// does not treat our Hangup as the client leaving.
func (out *outputTransport) ProcessFrame(ctx context.Context, f frames.Frame, dir processor.Direction) error {
	if err := out.BaseOutput.ProcessFrame(ctx, f, dir); err != nil {
		return err
	}
	if dir != processor.Downstream {
		return nil
	}
	switch f.(type) {
	case *frames.EndFrame, *frames.CancelFrame:
		out.bye(ctx)
	}
	return nil
}

func (out *outputTransport) bye(ctx context.Context) {
	out.once.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		if out.closing != nil {
			out.closing.Store(true)
		}
		if out.call != nil {
			_ = out.call.Hangup(ctx)
		}
	})
}

// StartWriting is the base's cue that WriteAudio may queue PCM. The base
// sender is what drains WriteAudio into WritePCM.
func (out *outputTransport) StartWriting(context.Context) error { return nil }

// SupportsNativeDTMF reports that keys travel as telephone-event, not audio.
func (out *outputTransport) SupportsNativeDTMF() bool { return true }

// WriteDTMFNative maps keypad keys to events 0-11 and sends them with
// Call.WriteDTMF. The base calls this immediately for an urgent frame and
// only after queued audio for a normal frame.
func (out *outputTransport) WriteDTMFNative(ctx context.Context, f frames.DTMFOutput) error {
	if out.call == nil {
		return ErrMediaClosed
	}
	for _, key := range f.Keys() {
		if err := ctx.Err(); err != nil {
			return err
		}
		ev, ok := eventForKey(key)
		if !ok {
			continue
		}
		if err := out.call.WriteDTMF(DTMF{Event: ev}); err != nil {
			return err
		}
	}
	return nil
}

// WriteAudio converts pipeline PCM to 8000 Hz and queues it on the call.
func (out *outputTransport) WriteAudio(ctx context.Context, f frames.OutputAudioFrame) (bool, error) {
	if out.call == nil || f == nil || f.AudioData() == nil {
		return false, nil
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-out.call.Done():
		return false, nil
	default:
	}
	pcm := f.AudioData().Audio
	if out.pipe != nil {
		pcm = out.pipe.FromPipeline(pcm)
	}
	if len(pcm) == 0 {
		return true, nil
	}
	if err := out.call.WritePCM(pcm); err != nil {
		return false, err
	}
	return true, nil
}

func eventForKey(key frames.KeypadEntry) (uint8, bool) {
	switch key {
	case frames.KeypadZero:
		return 0, true
	case frames.KeypadOne:
		return 1, true
	case frames.KeypadTwo:
		return 2, true
	case frames.KeypadThree:
		return 3, true
	case frames.KeypadFour:
		return 4, true
	case frames.KeypadFive:
		return 5, true
	case frames.KeypadSix:
		return 6, true
	case frames.KeypadSeven:
		return 7, true
	case frames.KeypadEight:
		return 8, true
	case frames.KeypadNine:
		return 9, true
	case frames.KeypadStar:
		return 10, true
	case frames.KeypadPound:
		return 11, true
	default:
		return 0, false
	}
}

func buttonForEvent(event uint8) (frames.KeypadEntry, bool) {
	switch event {
	case 0:
		return frames.KeypadZero, true
	case 1:
		return frames.KeypadOne, true
	case 2:
		return frames.KeypadTwo, true
	case 3:
		return frames.KeypadThree, true
	case 4:
		return frames.KeypadFour, true
	case 5:
		return frames.KeypadFive, true
	case 6:
		return frames.KeypadSix, true
	case 7:
		return frames.KeypadSeven, true
	case 8:
		return frames.KeypadEight, true
	case 9:
		return frames.KeypadNine, true
	case 10:
		return frames.KeypadStar, true
	case 11:
		return frames.KeypadPound, true
	default:
		return "", false
	}
}

var (
	_ transport.Transport    = (*Transport)(nil)
	_ transport.InputDriver  = (*inputTransport)(nil)
	_ transport.OutputDriver = (*outputTransport)(nil)
)
