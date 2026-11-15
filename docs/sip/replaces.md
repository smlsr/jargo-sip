# Replaces

Owner: JAR-9. RFC 3891. `OnReplaced` is the application hook:

```go
func (e *Endpoint) OnReplaced(func(old, new *Call))
```

One callback is stored. A later call replaces it. A nil callback clears it. `OnReplaced(old, new)` runs before the old call's client-disconnected event ([pipeline-adapter.md](pipeline-adapter.md)). The new call is established. The old call then receives BYE processing and `Done` closes.

## Tags on an attended REFER

The party who receives the new INVITE is the remote target of `other`. That party matches Replaces against its own dialog. On our `other` call:

- `to-tag` is `other.Remote().RemoteTag` (their local tag).
- `from-tag` is `other.Remote().LocalTag` (their remote tag, which is our tag).
- The Call-ID is `other.Remote().CallID`.

The `Refer-To` URI is their Contact, with those Replaces parameters embedded. `early-only` is included only when `other` is still early. `TransferAttended` refuses a call that is not established, so the attended REFER we send does not set `early-only`.

## INVITE that carries Replaces

We are the target. Match Call-ID and tags to a dialog in our call table.

- No dialog, or a dialog that is already ending: `481`. The new INVITE does not proceed.
- A confirmed dialog: answer the new INVITE as a normal call ([inbound-dialog.md](inbound-dialog.md)), BYE the named dialog, and fire `OnReplaced(old, new)`.
- An early dialog: `491`, unless the Replaces header has `early-only` and the dialog is still early. In that case answer the new call and send `487` on the old one, then fire `OnReplaced`.

Answering "as a normal call" still enforces `AllowFrom`, digest when `RequireAuth` is set, and the codec rules. A `488` or a failed challenge does not BYE the named dialog and does not fire `OnReplaced`.

## Outbound INVITE from an inbound REFER

When we `Dial` a `Refer-To` that already contains Replaces, copy that header onto the INVITE. We do not rewrite its tags. Success and failure follow [refer.md](refer.md).
