# re-INVITE

Owner: JAR-8. In the first build, together with UPDATE. One offer/answer lock covers both.

A re-INVITE uses the same Call-ID and the same tags. Our CSeq is one more than the last request we sent or received on the dialog. `o=` version increments on every offer we send.

With SDP, we answer in the `200` and wait for ACK before the media change is committed. Without SDP, the `200` carries our current SDP and the media stays. Store the new Contact when the request has one.

Media moves only when `c=` or `m=` changed. A new address resets the latch to `SDPTarget`. Local sockets, SSRC, and sequence stay. The codec we offered applies on the next sender tick. A codec we did not offer is `488` and the previous session stays.

## Direction

The direction we apply:

| Direction | Playout (`ReadPCM` caller audio) | Sender |
| --- | --- | --- |
| `sendonly` | stopped | silence, sequence unbroken |
| `recvonly` | kept | silence |
| `inactive` | stopped | silence |
| `sendrecv` | restored | restored |

Stopped playout means `ReadPCM` returns concealment frames rather than the caller's audio. Silence on the sender is µ-law `0xFF` or A-law `0xD5`, one packet per 20 ms, sequence and timestamp still advancing.

JAR-8: a re-INVITE `sendonly` stops `ReadPCM` caller audio and the sender continues silence with the sequence unbroken.

`Hold` and `Resume` prefer UPDATE when the peer's Allow contains UPDATE. Otherwise they send a re-INVITE. `Hold` offers `a=sendonly`. `Resume` offers `a=sendrecv`. Our answer to a peer offer uses the same direction table for the attribute they sent.

## One offer at a time

A second offer while one is unanswered gets `491 Request Pending`. That includes a re-INVITE or an UPDATE arriving while we still owe or still wait for an ACK.

On our own collision (we send an offer and receive one before the exchange finishes), retry once. Wait 2.1 s to 4 s when we are the UAS, and 0 s to 2 s when we are the UAC (RFC 3261 §14.2). The wait is chosen inside that range. Exactly one retry runs. A second collision fails the operation that we started (`Hold`, `Resume`, or the session refresh) and leaves the previous media in place.

JAR-8: crossing re-INVITEs produce a 491 and exactly one retry completes.
