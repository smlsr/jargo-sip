# UPDATE

Owner: JAR-8. RFC 3311. UPDATE is in the first build. The answer is the response. There is no ACK.

UPDATE is legal in an early dialog and in a confirmed dialog. Early dialogs include the outbound call after a reliable or unreliable 183 and before 200 ([early-media.md](early-media.md)).

We do not send UPDATE while an INVITE offer is open. Queue that UPDATE until ACK, then send it. An inbound UPDATE that arrives during an open offer is `491 Request Pending`. When we are the ones generating the open offer, respond `500` and include `Retry-After`. The delay is inside the UAC collision window, 0 to 2 seconds.

No SDP on an inbound UPDATE: `200`, and the media is untouched. With SDP: the same direction, latch, and codec rules as re-INVITE ([reinvite.md](reinvite.md)). A changed `c=` or `m=` resets the latch to `SDPTarget` and does not rebind. A codec we did not offer is `488` and the previous session stays.

`Hold` and `Resume` send UPDATE when the peer allowed it. UPDATE hold is `a=sendonly` from us, with no ACK. JAR-8: UPDATE hold stops `ReadPCM` caller audio the same way re-INVITE `sendonly` does, and the sender continues silence with the sequence unbroken. An UPDATE that changes the RTP port resets the latch.

Session-timer refresh uses UPDATE the same way when UPDATE is allowed ([session-timers.md](session-timers.md)). A refresh that does not intend a media change sends UPDATE with no SDP.
