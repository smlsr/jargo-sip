# RTP sender

Owner: JAR-5, with the clock rules tested again in JAR-6.

One goroutine, a 20 ms ticker, a monotonic clock. A late tick sends one packet. It does not burst a catch-up train.

The SSRC is random and constant for the life of the local sockets. Re-INVITE and UPDATE keep it. The initial sequence number and the initial timestamp are random (RFC 3550). The timestamp advances by the sample count of every packet, including silence and including the packets sent while held. A 20 ms G.711 packet advances the timestamp by 160. A 10 ms packet, if one is ever sent, advances it by 80. This build's sender ticks at 20 ms, so the advance on the sender is 160.

The marker bit is 1 on the first packet of a talkspurt and 0 on the rest. Silence after hold, and the first audio packet after silence, starts a talkspurt. Comfort-noise payload 13 is never sent.

The payload is one G.711 frame from [g711-resample.md](g711-resample.md). `WritePCM` only fills the queue. The sender owns the clock and reads 320 PCM bytes (160 samples) per tick. When the queue is short, the tick sends codec silence and still advances sequence and timestamp.

Sequence numbers are unbroken across hold, silence, and DTMF. DTMF replaces the audio payload for its packets and still consumes one sequence number per 20 ms ([dtmf.md](dtmf.md)).

The destination is the latch's current `WriteTo` address ([latch.md](latch.md)). A latch move changes that address on the next tick. The socket is not rebound.

Pion RTP is `github.com/pion/rtp` v1.10.6. Packets are version 2, payload type of the negotiated codec, no header extensions, no padding required.
