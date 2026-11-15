# SDP

Owner: JAR-4. Round-trip the offer we generate and a SIPp offer. No network in that story. Addresses and ports come from [nat-stun.md](nat-stun.md) and [ports.md](ports.md).

## Offer we send

```text
v=0
o=- <sess> <vers> IN IP4 <advertiseIP>
s=-
c=IN IP4 <advertiseIP>
t=0 0
m=audio <rtpPort> RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-15
a=ptime:20
a=sendrecv
```

`<sess>` is a random session id, chosen once per call, stable for the life of the call. `<vers>` starts at 1 and increases by 1 on every offer we send (RFC 3264). The answer we send does not bump `<vers>` unless we are the offerer of that exchange.

Codec order follows `Config.Codecs`. The default is PCMU then PCMA, so the `m=` line lists `0` then `8`. An offer that contains only the codecs we are willing to send still lists telephone-event. Payload 101 is the default dynamic type (`DTMF.PayloadType` zero means 101). `a=fmtp:101 0-15` matches that type.

`a=ptime:20`. Direction on a normal offer is `a=sendrecv`. Hold changes it; see [reinvite.md](reinvite.md).

There is no `a=rtcp-mux`. RTCP is the RTP port plus 1. Add `a=rtcp:<port>` only when the two ports are not consecutive, which is the `PortMap: stun` case. When they are consecutive, omit `a=rtcp`.

Absent from every SDP we send: SRTP, ICE, DTLS, Opus, video, T.38, and comfort-noise payload 13.

## Answer we send

On an inbound offer, choose the first entry of `Config.Codecs` that the offer lists. PCMU is payload 0. PCMA is payload 8. If the offer has neither, respond 488 and leave the previous session in place on a re-INVITE or UPDATE. On the initial INVITE, 488 ends the attempt and the sockets are freed.

Take `telephone-event/8000` at the offer's dynamic payload type. That type is the one we send and accept. A missing `a=fmtp` means events 0-15. Another clock on telephone-event is ignored. A missing telephone-event line still connects. `WriteDTMF` then returns `ErrDTMFNotNegotiated`.

The answer lists the chosen audio codec and, when we accepted telephone-event, that event type. `a=ptime:20`. Direction mirrors the offer using the table in [reinvite.md](reinvite.md).

## Answer we receive

The answer's payload type is what we send and accept, including a telephone-event type other than 101. A codec we did not offer is 488 on a re-offer we receive; on our own INVITE, an answer with neither PCMU nor PCMA fails the dial. Store the connection address, the RTP port, and `a=rtcp` when it is present. Those are the SDP target until the latch moves ([latch.md](latch.md)).

`o=` from the peer is stored. A new offer from the peer with a higher version, or with a changed `c=` or `m=`, is a media change even when the direction is unchanged.
