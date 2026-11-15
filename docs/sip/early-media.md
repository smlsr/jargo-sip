# 183 early media

Owner: JAR-7. This build plays 183 early media into the pipeline. A 183 with SDP is media, not a signal to ignore.

## Outbound

`Dial` is waiting for `200`. A `183` with SDP:

1. Parses the SDP with the same rules as a `200` answer ([sdp.md](sdp.md)).
2. Remembers that RTP address as the SDP target.
3. Starts the receiver, the latch, the jitter buffer, and `ReadPCM` against it.
4. Keeps every decoded frame, in order, for the pipeline.

If a later `200` has a different `c=` or `m=`, the latch returns to `SDPTarget` for the new address, the same way a re-INVITE does. SSRC and sequence of our sender stay if we have already sent. If the `200` repeats the same address, the latch state stays.

`Dial` still returns the `Call` only after ACK on `200`. The early frames are the first frames `ReadPCM` returns. They are not dropped and they are not time-compressed: each is a 20 ms frame the playout clock already produced. The pipeline adapter pushes those frames as soon as it starts reading ([pipeline-adapter.md](pipeline-adapter.md)).

A `183` without SDP updates nothing and plays nothing. A reliable `183` is also PRACKed ([100rel.md](100rel.md)). Play does not wait for the PRACK response.

We send RTP only after we have an address. On early media we may send codec silence to the 183 address so the peer's latch can learn us, using the sender rules in [rtp.md](rtp.md). Confirmed media on `200` keeps that sender.

## Inbound

The basic UAS path is `100` then `200`. We do not generate a `183` on that path. Receiving early media is the UAC behavior above. When a peer sends us UPDATE in the early dialog, [update.md](update.md) applies.

## Acceptance

JAR-7: a `183` with SDP that carries PCMU becomes `ReadPCM` frames (320 bytes, 8000 Hz) and, once `NewTransport` is attached, `InputAudioRawFrame`s. None of those frames are discarded because they arrived before `200`.
