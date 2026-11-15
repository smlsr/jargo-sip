# Pipeline adapter

Owner: JAR-10. Blocked on JAR-7 and JAR-9.

```go
func NewTransport(c *Call, p transport.Params) *Transport
```

`transport.Params` is `github.com/gojargo/jargo/transport.Params` from jargo v0.6.1. `Transport` implements that module's `transport.Transport`: `Input`, `Output`, and `Events`. The shape matches `transport/rtc`. `NewTransport` there takes a `*Connection`. This one takes a `*Call`.

The adapter file is the only file that imports `github.com/gojargo/jargo/transport`, `github.com/gojargo/jargo/frames`, and `github.com/gojargo/jargo/processor`. `Endpoint` and `Call` do not mention those types. Codec code may import `audio`, `audio/g711`, and `audio/resample` ([g711-resample.md](g711-resample.md)).

## Construction

`NewTransport` embeds `BaseInput` and `BaseOutput` the way `rtc.inputTransport` and `rtc.outputTransport` do.

- `AudioOut10msChunks` is 2. `NewTransport` sets 2 so the base hands over 20 ms, which is one G.711 frame.
- `AudioOutAutoSilence` is left nil, so `AudioOutFillsSilence` stays on. The RTP sender's own silence is still codec silence (`0xFF` / `0xD5`) when the queue is empty ([latch.md](latch.md)).
- A zero `AudioInSampleRate` or `AudioOutSampleRate` becomes 8000. A non-zero rate is the pipeline rate for that direction.
- Channels are 1. `NewTransport` forces mono when the param is 0 or any other count. This build is mono.
- Events registered: `EventClientConnected`, `EventClientDisconnected`, `EventAppMessage`. The client value on the first two is the `*Call`.

`StartReading` starts the playout loop: each `ReadPCM` becomes `frames.NewInputAudioRawFrame(pcm, rate, 1)` at the pipeline input rate, passed to `PushAudioFrame`. `StartWriting` starts draining `WriteAudio` into `WritePCM`.

When the call is already producing early-media frames, the read loop emits them first ([early-media.md](early-media.md)).

## Rates

One resample stream per direction, and only when that direction's pipeline rate differs from 8000. `ClearAfter` is `resample.NeverClear`. Equal rates copy. Details and the JAR-6 checks are in [g711-resample.md](g711-resample.md).

## DTMF

`SupportsNativeDTMF` returns true. `WriteDTMFNative` sends the keys. They are not mixed into the audio.

`frames.KeypadEntry` maps to telephone-event:

| Key | Event |
| --- | --- |
| `0`–`9` | 0–9 |
| `*` | 10 |
| `#` | 11 |

`OutputDTMFFrame` waits its turn in the audio queue. `OutputDTMFUrgentFrame` cuts in on the next sender tick ([dtmf.md](dtmf.md)). Both call `Call.WriteDTMF`.

Inbound events 0–11 become `frames.NewInputDTMFFrame`, pushed as a system frame so a barge-in does not drop them. Events 12–15 and 16 stay on `Call.ReadDTMF` and are not keypad frames.

## Hang-up

`EndFrame` and `CancelFrame` send BYE through `Call.Hangup`. `EndFrame` lets the base drain queued audio first, then hangs up. `CancelFrame` hangs up without that wait. The same split as `rtc.outputTransport`.

A remote BYE closes the `Call`. The adapter raises `EventClientDisconnected` with the `*Call` and does not treat our own `Hangup` as the client leaving (`closing` is set first, as in rtc).

`OnReplaced` fires before that disconnect. The adapter's order is: run the endpoint callback, then raise `EventClientDisconnected` for the old call. The new `Call` is what the application attaches next. This adapter does not silently swap the `Call` under a live `Transport`.

`Done` closing for any other reason (session timer, `MaxCall`, `Endpoint.Close`) also raises client disconnect when we were not the side that already set `closing`.
