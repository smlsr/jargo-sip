# REFER

Owner: JAR-9. RFC 3515. NOTIFY bodies are `message/sipfrag`. Subscription state is `active` on the intermediate notifies and `terminated;reason=noresource` on the final one.

`Transfer` is a blind REFER. `TransferAttended` is an attended REFER. Both require an established call. `Transfer` returns after the outcome below, or when the subscription expires with no NOTIFY.

## Blind

`Refer-To` is the target URI. It carries no Replaces. We expect `202`, then NOTIFY. A sipfrag of `SIP/2.0 200` means BYE the original dialog. The new dialog, if we are also the party that was dialed, is a separate call. When we are only the transferor, the original `Call` ends and `Transfer` returns nil.

A `4xx` or `5xx` sipfrag, or no NOTIFY before the subscription expires, leaves the original call up and `Transfer` returns an error.

JAR-9: blind REFER ends with NOTIFY 200, BYE on the original, and a live call on the new one. The live call is the dialog the notifier established. When that dialog is ours because the target reached us, `OnReplaced` delivers it ([replaces.md](replaces.md)). When the target is outside this process, the NOTIFY 200 and the BYE are the local success, and the new call is the peer's.

## Attended

`other` must be established. Otherwise `TransferAttended` returns an error and sends nothing. `Refer-To` is `other`'s remote target (the Contact in `other.Remote().RemoteTarget`) with an embedded Replaces header. The tags are the tags as seen by the party who receives the new INVITE. See [replaces.md](replaces.md). We add `Referred-By` (RFC 3892) naming our AoR, `sip:User@Domain`.

The NOTIFY watch and the BYE-on-success rule are the same as blind. JAR-9: the attended REFER carries Replaces.

## Inbound REFER

A `Refer-To` whose method is anything other than INVITE, or a nested URI we cannot parse, is `400`. We do not send `202`.

Otherwise:

1. `202 Accepted`.
2. NOTIFY `SIP/2.0 100 Trying` immediately, `Subscription-State: active`.
3. `Dial` the `Refer-To` URI ([outbound-dialog.md](outbound-dialog.md)). When the URI contains Replaces, the outbound INVITE carries that Replaces header.
4. Further NOTIFY sipfrags track the dial (`180`, `183`, the final status).
5. On `200` of the new dialog: NOTIFY `SIP/2.0 200`, `Subscription-State: terminated;reason=noresource`, BYE the original dialog, then `OnReplaced(old, new)` before the old call's client-disconnected event.
6. On failure: NOTIFY that status, `terminated;reason=noresource`, leave the original call up, and do not fire `OnReplaced`.

The original `Call` stays readable until the BYE finishes.
