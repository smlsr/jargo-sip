# re-INVITE

Owner: JAR-8. In the first build, together with UPDATE. One offer/answer lock covers both.

A re-INVITE uses the same Call-ID and the same tags. CSeq is per direction. A request we send is one higher than the last request we sent. A request we receive updates only the remote sequence. ACK and CANCEL copy the INVITE CSeq and do not increment. `o=` version increments on every offer we send.

With SDP, we answer in the `200` and commit the media change when that `200` is sent. The first INVITE still starts media on ACK ([inbound-dialog.md](inbound-dialog.md)). Without SDP, the `200` carries our current SDP and the media stays. Store the new Contact when the request has one.

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

An INVITE that arrives while an INVITE we sent is unanswered is `491 Request Pending` (RFC 3261 §14.2). Crossing re-INVITEs are this 491 case. A second INVITE that arrives before we have sent the final response to an INVITE we received is `500` with `Retry-After` chosen at random from 0 to 10 seconds.

On a 491, retry once. The wait follows Call-ID ownership (RFC 3261 §14.1, the timer §14.2 uses). The UA that owns the Call-ID waits 2.1 to 4 seconds. The other UA waits 0 to 2 seconds. The wait is chosen inside that range. Exactly one retry runs. A second collision fails the operation that we started (`Hold`, `Resume`, or the session refresh) and leaves the previous media in place.

UPDATE glare is the split in [update.md](update.md). An offer while we generated an offer that is still unanswered is `491`. An offer while we received an offer we have not answered is `500` with `Retry-After` random from 0 to 10 seconds. An UPDATE before we have sent a final response to a previous UPDATE is that same `500`.

JAR-8: crossing re-INVITEs produce a 491 and exactly one retry completes.
