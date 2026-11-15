# OPTIONS

Owners: JAR-8 for the response on a dialog, JAR-11 for a registrar's probe.

OPTIONS is not a media operation. It does not allocate a port, move the latch, reset jitter, or change SDP.

An OPTIONS request, in a dialog or outside one, gets `200` with the same Allow list as every other response:

```text
Allow: INVITE, ACK, CANCEL, BYE, UPDATE, PRACK, REFER, NOTIFY, OPTIONS
```

`Supported` matches [config.md](config.md) (`100rel`, and `timer` when session timers are on). `Accept: application/sdp`. The body is empty. `AllowFrom` applies: a source that is not listed is dropped with no response, the same as an INVITE.

We do not send OPTIONS as a keepalive or as a session refresh. Refresh is UPDATE or re-INVITE ([session-timers.md](session-timers.md)).

JAR-11: a test registrar's OPTIONS receives this `200`.
