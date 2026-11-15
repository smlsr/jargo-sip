# RTCP

Owner: JAR-5. Library: `github.com/pion/rtcp` v1.2.20. Compound packets. A separate socket, the RTP port plus 1, or the mapped RTCP port when `PortMap` is `stun`.

The ticker interval is `RTCP.Interval`, or 5 s when that is zero. A report is also sent when there is something to report (a new peer SR to acknowledge, or an RTCP BYE to answer with our own bye on hang-up). The ticker runs off the audio goroutine. It does not stall the RTP sender or the playout tick.

## What we send

- After we have sent RTP: a Sender Report, plus a Receiver Report when we have received RTP, plus SDES CNAME `user@domain` (`Config.User` and `Config.Domain`).
- Before we have sent RTP: a Receiver Report and the same SDES, once we have received something. Until then, send nothing but keep the ticker armed.

Fraction lost and cumulative lost come from the jitter buffer. Jitter is the RFC 3550 interarrival estimate. LSR and DLSR come from the last SR we received. RTT is computed from those and logged. It is not applied to the jitter target.

JAR-5: two localhost sessions, and an RTCP SR shows up on the paired port.

## What we accept

A compound packet is valid when version is 2 and the first packet type is 200 through 204. Anything else is dropped. RTP arriving on the RTCP socket is logged once per call and then ignored.

## RTCP latch

Independent of the RTP latch. Until the RTCP latch commits, send to `a=rtcp` when the SDP has it, otherwise to the RTP port plus 1 on the SDP connection address.

The first valid compound packet is provisional, and the sender retargets. Commit is 2 packets from that source, or 100 ms with no contradicting source. After commit the RTCP destination is frozen until a re-INVITE or UPDATE changes `c=` or `m=`, which returns this latch to the SDP target too.

An RTCP BYE is logged and surfaced on the call (a counter and a log line the tests can read). It does not hang up. SIP BYE is the only hang-up.
