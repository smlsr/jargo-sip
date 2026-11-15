# Custom headers

Owner: JAR-13. Blocked on the dialog, transfer, and REGISTER stack (JAR-11). Amendment: `internal/sip-plan.md`, section "Custom headers".

Every request and response can carry caller-supplied headers. Every received message exposes the headers that arrived. The same read and set path covers INVITE, ACK, CANCEL, BYE, OPTIONS, REGISTER, PRACK, UPDATE, REFER, and NOTIFY.

```go
type Header struct {
    Name  string
    Value string
}

func (c *Call) SetHeader(name, value string)
func (c *Call) Header(name string) (string, bool)

func (e *Endpoint) SetHeader(name, value string)
func (e *Endpoint) Header(name string) (string, bool)

func (e *Endpoint) Dial(ctx context.Context, uri string, headers ...Header) (*Call, error)
func (c *Call) Transfer(ctx context.Context, target string, headers ...Header) error
func (c *Call) TransferAttended(ctx context.Context, other *Call, headers ...Header) error
```

Names compare case-insensitively. `SetHeader("X-Trace", "a")` and a later read of `"x-trace"` are the same field. A second `SetHeader` for that name replaces the value.

## Owned hop-by-hop headers

These names stay the stack's. A caller value does not replace them, on a request or a response:

`Via`, `Contact`, `CSeq`, `Call-ID`, `From`, `To`, `Max-Forwards`, `Content-Length`, `Route`, `Record-Route`.

`SetHeader` with one of those names stores nothing. The same refusal applies to a header passed to `Dial`, `Transfer`, or `TransferAttended`.

## What a caller sets

`SetHeader` on a `Call` is applied to later requests and responses that call sends: BYE, UPDATE, re-INVITE, PRACK, NOTIFY, OPTIONS, and the rest of the dialog methods.

`SetHeader` on an `Endpoint` is applied to requests outside a dialog. REGISTER is the one this build sends. It is also applied to a response when the dialog has not set the same name.

A header passed to `Dial` is stored on that call before the INVITE is built. It is on the INVITE, and it is copied onto that INVITE's ACK and onto its CANCEL. A digest retry reuses the same INVITE, so the header stays. Owned names in that list are still refused.

A header passed to `Transfer` or `TransferAttended` is on that REFER. It is not kept for a later BYE.

## What arrives

The headers of an inbound message are copied onto the `Call` before the handler acts:

- INVITE, before `Accept` returns that call
- re-INVITE and UPDATE, before the offer is answered
- NOTIFY, before `Transfer` observes the sipfrag
- BYE, OPTIONS, and PRACK, before the response is sent
- responses, including 183 and the final response to `Dial`, before early media, ACK, or the call is returned

`Header` returns the value from the newest arrived message that carried that name. ACK and CANCEL do not replace the INVITE headers `Accept` exposes.

REGISTER has no `Call`. The response is copied onto the `Endpoint` before `Start` returns, and before a digest challenge is answered. `Endpoint.Header` reads it. An OPTIONS outside a dialog is exposed the same way.
