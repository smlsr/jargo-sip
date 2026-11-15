# Ports

Owner: JAR-4. Depends on the range checks in [config.md](config.md).

RTP ports are even and lie in `[RTPPortMin, RTPPortMax]` after zero defaults (10000 and 20000). Both ends of the configured range are even. `RTPPortMin` greater than `RTPPortMax` is inverted and `Start` refuses it. Either end odd is refused. The range must contain at least one even port.

The allocator hands out the lowest free even port. The call binds that port for RTP and the next port (RTP+1) for RTCP. Both binds use `ListenIP` (or `0.0.0.0` when it is empty) and UDP. A port that will not bind is skipped. When no even port is free, setup fails and no SIP success response is sent.

`standalone` with `PortMap: stun` sends a Binding from each of those two sockets before the SDP is written. The local ports stay the ones we bound. The advertised ports are the mapped ports. See [nat-stun.md](nat-stun.md).

Sockets are freed when:

- CANCEL arrives before 200 (we answer 487).
- The INVITE fails (488, 401 never answered, dial error).
- BYE completes, in either direction.
- `Hangup`, `Endpoint.Close`, `MaxCall`, or session-timer expiry.

A freed even port returns to the pool. Two live calls do not share a port. Re-INVITE and UPDATE do not allocate a new local port and do not rebind. Only the destination `WriteTo` address changes.

JAR-4 parses our offer and a SIPp offer with no network. The SIPp offer fixture uses a non-zero connection address and an even RTP port. Our parser keeps PCMU, PCMA, and telephone-event/8000, and records `a=rtcp` when the fixture has one.
