# jargo-sip

A standalone Go SIP user-agent library. jargo uses it to terminate telephony trunks: one process answers and places calls, and each call is one dialog plus one RTP session.

The module is `github.com/smlsr/jargo-sip`. The library package is [`transport/sip`](transport/sip). Signaling transactions and dialogs go through [sipgo](https://github.com/emiago/sipgo). Media is G.711 on [pion/rtp](https://github.com/pion/rtp) and [pion/rtcp](https://github.com/pion/rtcp). `Endpoint` is one SIP port and many calls. `Call` is one dialog and one RTP session, with PCM in and out.

The product contract is [docs/sip/spec.md](docs/sip/spec.md). Live SIPp checks are [docs/sip/sipp.md](docs/sip/sipp.md). The page index is [docs/sip/README.md](docs/sip/README.md).

## What it does

- **Inbound calls.** `Accept` returns a `*Call` after the ACK, once media has started. An INVITE whose source is outside `AllowFrom` is dropped. `RequireAuth` with `Auth` answers that INVITE with 401 until the digest succeeds.
- **Outbound calls.** `Dial` sends one INVITE and returns the `*Call` after ACK on 200. A 401 or 407 is retried once when `Auth` is set. A second challenge fails the dial.
- **SDP.** An offer lists `Config.Codecs` (PCMU, then PCMA, when `Codecs` is empty) and `telephone-event/8000`, with `a=ptime:20`. An answer uses the first of those codecs that the offer contains, and includes `telephone-event` when the offer has it at 8000 Hz.
- **Symmetric RTP latching.** Audio is sent to the SDP address until a matching RTP source is learned. Behind an SBC the default match is the SDP address's IP (the port may differ). In standalone mode the default match is that packet's IP and port. `Latch.Match` can select `port`, `address`, or `off`. After the source commits, the send address stays. A re-INVITE or UPDATE that changes `c=` or `m=` points the latch back at the new SDP target. The local sockets stay bound.
- **RTCP.** Reports go out on the paired socket, RTP port + 1, as compound sender and receiver reports. An RTCP BYE is recorded on the call. A SIP BYE ends it.
- **RFC 4733 DTMF.** `ReadDTMF` returns one press. `WriteDTMF` replaces audio for that press, then sends three end packets. Offers use payload type 101. An inbound offer's dynamic type at 8000 Hz wins. `WriteDTMF` returns `ErrDTMFNotNegotiated` when telephone-event was not negotiated, and the call stays up.
- **G.711.** `ReadPCM` returns one 20 ms frame: 160 samples, 320 bytes, 16-bit little-endian, at 8000 Hz. An underrun returns one concealment frame and a nil error. `WritePCM` queues 8000 Hz PCM. The sender owns the 20 ms clock. The default codec list is PCMU, then PCMA.
- **183 early media.** A 183 with SDP on an outbound call is decoded into `ReadPCM`. Those frames are the first ones `ReadPCM` returns. `Dial` still returns only after ACK on 200.
- **100rel / PRACK (RFC 3262).** The INVITE advertises `Supported: 100rel`. A reliable provisional is answered with PRACK. A reliable 183 is played and PRACKed.
- **REFER.** `Transfer` is a blind REFER (RFC 3515). `TransferAttended` is an attended REFER whose Refer-To carries Replaces for another established `Call`. NOTIFY 200 ends the original dialog. `OnReplaced` runs before that call's `Done` channel closes.
- **Session refresh (RFC 4028).** Set `SessionTimer` to a `time.Duration`. The stack refreshes at half the agreed interval, by UPDATE when the peer allows it, otherwise by re-INVITE. A zero value leaves the timer off. Expiry sends BYE.
- **OPTIONS.** An incoming OPTIONS, in a dialog or outside one, is answered with 200 and the same `Allow` list as other responses.
- **Custom headers.** `Dial`, `Transfer`, and `TransferAttended` take `Header` values. `Call.SetHeader` and `Endpoint.SetHeader` apply to later requests. `Header` reads the latest value that arrived for that name. Names compare case-insensitively. Via, Contact, CSeq, Call-ID, From, To, Max-Forwards, Content-Length, Route, and Record-Route stay the stack's.
- **SIP TLS.** List `"tls"` in `Transports` and set `Config.TLS`. See [TLS](#tls). RTP stays on UDP.
- **jargo transport.** `NewTransport` wraps one `Call` for a jargo pipeline, the same role `transport/rtc` fills for a WebRTC connection.

`Hold` and `Resume` offer `sendonly` and `sendrecv`. They send UPDATE when the peer allowed it, and re-INVITE otherwise.

`Config.Register` nil leaves REGISTER off. A non-nil value registers `sip:User@Domain` after the socket is up.

## Install

The `go` line in `go.mod` is 1.27.4.

```bash
go get github.com/smlsr/jargo-sip@latest
```

```go
import "github.com/smlsr/jargo-sip/transport/sip"
```

That module pulls `github.com/gojargo/jargo` v0.6.1, which `NewTransport` imports.

## Listen on UDP

An empty `Transports`, or `[]string{"udp"}`, listens on UDP. `SIPPort` zero is 5060. An empty `ListenIP` binds all IPv4 addresses. `NATBehindSBC` advertises the local address, so leave `AdvertiseIP` empty in that mode. `AllowFrom` is required. Each entry is an IP or a CIDR, matched against the source of the SIP packet. `Start` refuses an empty `Domain`, an empty `User`, or an empty `AllowFrom`. It also refuses `NATBehindSBC` combined with `AdvertiseIP`, `STUNServer`, or `PortMap` `"stun"`.

```go
package main

import (
	"context"
	"log"

	"github.com/smlsr/jargo-sip/transport/sip"
)

func main() {
	ctx := context.Background()
	ep, err := sip.New(sip.Config{
		NATMode:     sip.NATBehindSBC,
		Domain:      "pbx.example",
		User:        "agent",
		DisplayName: "Agent",
		SIPPort:     5060,
		Transports:  []string{"udp"},
		AllowFrom:   []string{"192.0.2.10"},
		Codecs:      []sip.Codec{sip.CodecPCMU, sip.CodecPCMA},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := ep.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer ep.Close()

	call, err := ep.Accept()
	if err != nil {
		log.Fatal(err)
	}
	defer call.Hangup(ctx)

	for {
		pcm, err := call.ReadPCM()
		if err != nil { // ErrMediaClosed after the dialog ends
			return
		}
		if err := call.WritePCM(pcm); err != nil {
			return
		}
	}
}
```

`Accept` blocks until one call is established. `Close` unblocks it with `ErrEndpointClosed`. `call.Done()` closes when the dialog ends. `call.Remote()` is the peer. `ReadPCM` returns a concealment frame, with a nil error, when the jitter buffer underruns, so the loop above writes that frame back.

To advertise a public address, set `NATMode` to `sip.NATStandalone` and set `AdvertiseIP`. With `AdvertiseIP` empty, `NATStandalone` uses a STUN Binding (RFC 8489) to `STUNServer` (`host:port`) from the SIP UDP socket.

```go
digit, err := call.ReadDTMF()
err = call.WriteDTMF(sip.DTMF{Event: 5}) // 0-9, 10 star, 11 hash. A zero Duration uses Config.DTMF.DigitDuration (160 ms).
```

## Dial

`Dial` uses a started `Endpoint`. The URI is `sip:` or `sips:`. Headers on this call are written on the INVITE and copied onto its ACK and CANCEL.

```go
call, err := ep.Dial(ctx, "sip:pstn@192.0.2.20:5060", sip.Header{
	Name:  "X-Account",
	Value: "42",
})
if err != nil {
	log.Fatal(err)
}
defer call.Hangup(ctx)

pcm, err := call.ReadPCM() // 183 audio, if any, comes out first
if err != nil {
	log.Fatal(err)
}
if err := call.WritePCM(pcm); err != nil {
	log.Fatal(err)
}
```

Set `Auth` when the trunk challenges. An empty `Username` uses `Config.User`.

```go
Auth: &sip.DigestAuth{Password: os.Getenv("SIP_PASSWORD")},
```

Blind and attended transfer:

```go
err = call.Transfer(ctx, "sip:queue@pbx.example")
err = call.TransferAttended(ctx, other) // other is an established *sip.Call
ep.OnReplaced(func(old, new *sip.Call) {})
```

`SessionTimer: 90 * time.Second` advertises that interval. Refresh runs at half the interval the peers agree. Zero leaves RFC 4028 off.

`call.SetHeader("X-Account", "42")` is applied to later requests on that call. `call.Header("X-Account")` returns the latest value that arrived.

## TLS

TLS configuration is `Config.TLS` (`*sip.TLSConfig`). `Start` requires it when `Transports` contains `"tls"`. `"udp"` and `"tls"` can both be listed. Contact carries `transport=udp` when the UDP socket is up, and `transport=tls` on a TLS-only listener. Media stays on UDP.

```go
cfg.Transports = []string{"tls"}
cfg.TLS = &sip.TLSConfig{
	CertFile: "cert.pem",
	KeyFile:  "key.pem",
}
```

`SelfSigned: true` with empty `CertFile` and `KeyFile` makes `Start` generate a certificate for the advertised address and use it on the TLS listener. When both files are set, those files are the certificate, including if `SelfSigned` is also set. `IgnoreCertWarnings: true` makes outbound TLS accept a peer certificate that fails verification, including a self-signed peer. With `IgnoreCertWarnings` false, verification stays on.

## jargo pipeline

`sip.NewTransport` adapts one `*Call` to a jargo pipeline. The `transport` import is `github.com/gojargo/jargo/transport`. A zero sample rate becomes 8000. `NewTransport` forces mono and sets the output chunk to 20 ms, one G.711 frame.

```go
params := transport.DefaultParams()
params.AudioInSampleRate = 8000
params.AudioOutSampleRate = 8000
tr := sip.NewTransport(call, params)
// tr.Input() and tr.Output() are the pipeline processors.
// tr.Events() reports client connect and disconnect. The value is the *Call.
```

A pipeline rate other than 8000 is resampled once, in that direction, at this edge. The trunk clock stays 8000 Hz G.711.

[examples/receptionist](examples/receptionist) is a full inbound pipeline. It listens on UDP, answers with `sip.NewTransport`, and speaks through Gemini. Trunk audio is 8 kHz G.711, PCMU then PCMA. The pipeline hears the caller at 16 kHz and speaks at 24 kHz. From the repository root:

```bash
go run ./examples/receptionist
```

The process reads `GEMINI_API_KEY` from the environment. When that variable is empty, it fills it from an untracked `.env` at `examples/receptionist/.env`, or from `.env` in the current directory. Copy [examples/receptionist/.env.example](examples/receptionist/.env.example) and put the key only in that untracked file or in the environment. Listener settings (`SIP_PORT`, `SIP_LISTEN_IP`, `SIP_DOMAIN`, and the rest) are in [examples/receptionist/README.md](examples/receptionist/README.md).

## Tests

`go test ./transport/sip` runs the package tests. The SIPp cases run when `sipp` is on `PATH`. What they send, and the `CAP_NET_RAW` requirement, is [docs/sip/sipp.md](docs/sip/sipp.md).

## License

[MIT](LICENSE).
