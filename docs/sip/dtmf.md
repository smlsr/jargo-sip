# DTMF

Owner: JAR-7. Events are RFC 4733 telephone-event. There are no in-band tones on the SIP leg.

## Negotiation

Offers we send use payload 101 unless `DTMF.PayloadType` is non-zero, with `a=fmtp:<pt> 0-15` and clock 8000. See [sdp.md](sdp.md).

On an inbound offer, the offer's dynamic type at 8000 wins. A missing fmtp means 0-15. Other clocks are ignored. A missing telephone-event line still connects. `WriteDTMF` returns `ErrDTMFNotNegotiated` and sends nothing.

## Events

| Event | Meaning | Reaches the pipeline as a key |
| --- | --- | --- |
| 0-9 | digits | yes |
| 10 | star | yes |
| 11 | hash | yes |
| 12-15 | A-D | no, `Call` only |
| 16 | flash | no, `Call` only |

An event outside the negotiated fmtp range is dropped.

`ReadDTMF` returns one event per press. A-D and flash are returned here and are not turned into `frames.InputDTMFFrame`. Keypad events are also returned here; the adapter maps 0-11 into keypad frames ([pipeline-adapter.md](pipeline-adapter.md)).

## Receive

Demux telephone-event before G.711 decode. The packet consumes its RTP sequence and is not jitter loss. The playout tick conceals that 20 ms.

Emit one `DTMF` when the end bit is first seen for the tuple (SSRC, timestamp, event), or when a new timestamp starts and the previous press never set the end bit. Retransmitted end packets do not emit again. The marker bit is not required.

JAR-7 with SIPp: telephone-event 5, three end packets, and `ReadDTMF` returns 5 once. A run that never sets the end bit still returns 5 once when the next timestamp starts. `Done` closes when the dialog ends.

## Send

`WriteDTMF` and `OutputDTMFUrgentFrame` both end up here. An urgent frame cuts in on the next 20 ms tick, ahead of queued PCM. A normal frame waits until PCM queued ahead of it has been sent.

The event replaces audio. The RTP timestamp stays at the start timestamp of the event. Every 20 ms the sequence number increments, the duration field grows by 160, and the end bit is clear. The default press is 160 ms (`DTMF.DigitDuration`), which is eight of those packets (duration 160, 320, …, 1280). Then three end packets with the end bit set, the final duration unchanged, the same timestamp, and the sequence still incrementing. The next audio timestamp is the event start plus the final duration. Volume is 10.

`InterDigitGap` (default 100 ms) is codec silence between keys, five 20 ms ticks, with the audio timestamp advancing. Sequence stays unbroken.

`DTMF.Duration` zero uses `DigitDuration`. A non-zero duration is rounded up to a 20 ms boundary. The three end packets are always sent.
