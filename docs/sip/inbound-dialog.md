# Inbound dialog

Owner: JAR-8. Media must already be able to move (JAR-5). sipgo v1.7.3 supplies transactions and the dialog session. It does not challenge with 401, and it does not implement our Allow list. We do both. The call table is on `Endpoint`.

`Accept` blocks until ACK on a 200, then returns that `Call`. Each established dialog is delivered to one `Accept`. Concurrent `Accept` callers each take a different call. `Close` makes `Accept` return an error.

## INVITE

1. Drop the packet unless the source address matches `AllowFrom`. No response to a source that is not allowed. Log the drop.
2. Answer `100 Trying`.
3. When `RequireAuth` is set, a request without a valid `Authorization` gets `401` and `WWW-Authenticate`, Digest, realm `Config.Domain`. Build and check the challenge with `github.com/icholy/digest` v1.2.0. sipgo does not do this step. A follow-up INVITE with bad credentials gets `403` and no second challenge. Credentials that check out continue. `Auth` holds the password we expect. `Start` already refused a nil `Auth` or an empty password.
4. Parse SDP. Neither PCMU nor PCMA: `488`, free the sockets.
5. Choose the codec and telephone-event type per [sdp.md](sdp.md).
6. Allocate an even RTP port and bind RTP and RTCP ([ports.md](ports.md)). `standalone` with `PortMap: stun` Binding-requests both sockets before the SDP is written.
7. `200 OK` with the answer SDP, the Contact, and the Allow list.
8. On ACK, start media toward the offer's RTP address ([latch.md](latch.md), [rtp.md](rtp.md)) and return the `Call` from `Accept`.

Until the latch commits, send to the SDP address.

CANCEL before 200 answers `487` and frees the sockets. No `Call` is delivered.

BYE after the dialog is established answers `200` and closes the `Call`. `Done` closes. The pipeline sees client disconnect only through the adapter ([pipeline-adapter.md](pipeline-adapter.md)).

A second INVITE for a dialog we already hold is a re-INVITE ([reinvite.md](reinvite.md)), not a new `Accept`.

## Digest once

The JAR acceptance "401 answered with digest exactly once" is the outbound side in [outbound-dialog.md](outbound-dialog.md) and, on this side, one 401 per unauthenticated INVITE. We do not send a second 401 for the same `Call-ID` after credentials have been offered.

## Headers on the dialog

We store Call-ID, local tag, remote tag, and the peer Contact as `Party.RemoteTarget`. CSeq on requests we send later is one more than the last CSeq on that dialog. re-INVITE uses the same Call-ID and tags ([reinvite.md](reinvite.md)).
