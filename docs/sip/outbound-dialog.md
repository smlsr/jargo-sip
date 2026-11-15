# Outbound dialog

Owner: JAR-9. `Dial(ctx, uri)` places one INVITE through a sipgo dialog client (`DialogClientSession` / `Client` in sipgo v1.7.3). The call table entry is ours.

## Steps

1. Allocate and bind RTP and RTCP ([ports.md](ports.md)). When `PortMap` is `stun`, Binding-request both sockets first ([nat-stun.md](nat-stun.md)).
2. Send INVITE. The body is the offer in [sdp.md](sdp.md): PCMU, PCMA, telephone-event/8000 at the configured dynamic type (default 101), `a=fmtp` `0-15`, `a=ptime:20`, `a=sendrecv`. From, Contact, and Via use [config.md](config.md). `Supported` includes `100rel`, and `timer` when session timers are on.
3. On `401` or `407`, apply digest once with sipgo `Client.DoDigestAuth` (or `TransactionDigestAuth`) and `Config.Auth`. The username is `Auth.Username` or `Config.User`. A second `401` or `407` fails the dial. That is the "digest exactly once" check. A nil `Auth` on a challenge fails the dial.
4. A `183` with SDP is early media ([early-media.md](early-media.md)). Remember the address and play it. This build plays it.
5. On `200`, parse the answer, send ACK, start confirmed media, and return the `Call`. The answer's payload types are what we send and accept.
6. A transport error or a timeout before `200` sends CANCEL and fails `Dial`. `ctx` cancellation does the same. Sockets are freed.
7. A final response other than `200` (`4xx`, `5xx`, `6xx`) fails `Dial` after the failure ACK for a non-2xx where the transaction requires one. Sockets are freed. No `Call` is returned.

`Dial` returns after ACK on `200`. Early media received while `Dial` is in progress is kept on that call and is the first audio `ReadPCM` yields, in order. It is not discarded. See [early-media.md](early-media.md).

The peer Allow list is stored. Hold, resume, and session refresh prefer UPDATE when that list contains UPDATE ([update.md](update.md)).
