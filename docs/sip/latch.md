# Latch

Owner: JAR-5. The peer's RTP address often differs from the address in SDP. The latch learns the address we write to. It does not rebind the local socket.

## States

`SDPTarget`, `Provisional`, `Committed`.

On establish, send RTP and RTCP to the SDP address. The RTP destination is SDP `c=` and the `m=` port. The RTCP destination is `a=rtcp` when present, otherwise the RTP port plus 1 on that connection address. Until the RTP latch commits, RTP goes to the SDP address. The RTCP latch is separate ([rtcp.md](rtcp.md)).

The first plausible RTP packet that passes `Latch.Match` moves the state to `Provisional`, and the sender retargets to that source immediately. That audio enters the jitter buffer. Commit happens on 3 packets from that 5-tuple, or on 100 ms with no contradicting source. Inside 500 ms of the first packet, a different source with more plausible packets switches the provisional target once. After that switch the 3-packet and 100 ms counters start again for the new tuple.

After `Committed` the address is frozen. Further sources are logged and dropped. A spoofed packet after commit does not move the latch. This is a JAR-5 acceptance check.

A re-INVITE or UPDATE whose `c=` or `m=` changed returns the latch to `SDPTarget`. The local sockets stay. SSRC, sequence, and timestamp stay. The jitter buffer resets because the remote address changed ([jitter.md](jitter.md)).

`Latch.Match: off` never leaves `SDPTarget`.

## Match

| `Match` | When it is the default | Rule |
| --- | --- | --- |
| `port` | `behind-sbc` when `Match` is empty | Source IP equals SDP `c=` or an `AllowRTPFrom` entry. The source port may differ. |
| `address` | `standalone` when `Match` is empty | Source IP and source port are taken from the packet. |
| `off` | never | Stay on the SDP target. |

`AllowRTPFrom` entries are IPs. They extend the `port` match. They do not apply to `address` or `off`.

## Plausible RTP

All of these hold:

- Version is 2.
- The payload type is the negotiated audio type or the negotiated telephone-event type.
- The SSRC is unseen, or it is the SSRC already accepted. A second SSRC is dropped.
- An audio payload is 160 bytes, or 80 bytes accepted as 10 ms. Any other audio length is not plausible.
- The packet is not STUN. STUN has magic cookie `0x2112A442` at byte 4.

Telephone-event packets are plausible for the latch and are demuxed before decode ([dtmf.md](dtmf.md)). They count toward the 3-packet commit.

## Sender before the pipeline

The sender starts at establishment with codec silence, µ-law `0xFF` or A-law `0xD5`, one 160-byte payload per 20 ms. That is not pipeline silence. `AudioOutFillsSilence` on the adapter stays on ([pipeline-adapter.md](pipeline-adapter.md)).
