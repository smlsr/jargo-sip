# Session timers

Owner: JAR-11. RFC 4028. `SessionTimer` zero disables the feature: we do not send `Session-Expires` or `Supported: timer`, and we do not refresh or expire on that clock. `MaxCall` is a separate cap ([config.md](config.md)).

When `SessionTimer` is set:

- The original INVITE carries `Session-Expires` with that duration and `refresher=uac` when we send it, plus `Supported: timer`.
- The agreed interval is the value on the `200`, or our offer when the `200` is silent. A peer `Min-SE` higher than our offer is adopted and the INVITE is retried once with that `Session-Expires`.
- We advertise `Session-Expires` and `Supported: timer` on the original INVITE. An inbound INVITE that asks for a timer is accepted. Our `200` echoes `Session-Expires` and names `refresher=uas` when the peer asked us to refresh, otherwise `refresher=uac`.

Refresh at half the agreed interval. Use UPDATE when the peer's Allow contains UPDATE, else re-INVITE ([update.md](update.md), [reinvite.md](reinvite.md)). A refresh does not change media: UPDATE carries no SDP, and a re-INVITE refresh carries the current SDP with the same `c=`, `m=`, and direction, so the latch stays. The `o=` version still increments on an offer we send.

If the refresh fails, or the interval elapses with no successful refresh, `Hangup` the call. Expiry is a SIP BYE. RTCP BYE is not the hang-up.

The timer runs per established call. Early media does not start it. ACK on `200` starts it.
