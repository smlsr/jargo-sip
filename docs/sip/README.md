# SIP endpoint specs

The product spec is [spec.md](spec.md).

Contract for `github.com/smlsr/jargo-sip`, package `transport/sip`. JAR-2 is documentation only. The amendment read for this set is `internal/sip-plan.md`. Where an older line said to drop 183 audio, [early media](early-media.md) wins: this build plays 183 into the pipeline.

`Endpoint` is one process, one SIP port, many calls. `Call` is one dialog, one RTP session, PCM in and out. `Transport` wraps one `Call` the way `github.com/gojargo/jargo/transport/rtc` wraps one `Connection`. There is no binary. Signaling is sipgo for transactions and dialogs only. Media is ours on `pion/rtp` and `pion/rtcp`, using G.711. sipgo `DialogServerCache` and `DialogClientCache` are examples; the call table lives on `Endpoint`.

## Index

| Page | What it specifies | Story |
| --- | --- | --- |
| [config.md](config.md) | `Config`, public `Endpoint` and `Call` API, `Start` refusals | JAR-3 |
| [nat-stun.md](nat-stun.md) | `behind-sbc` and `standalone`, advertise address, per-socket STUN | JAR-3 |
| [sdp.md](sdp.md) | Offers and answers we send and accept | JAR-4 |
| [ports.md](ports.md) | Even RTP allocation and the RTCP pair | JAR-4 |
| [latch.md](latch.md) | SDPTarget, Provisional, Committed | JAR-5 |
| [rtp.md](rtp.md) | 20 ms sender clock | JAR-5 |
| [jitter.md](jitter.md) | Playout ring and target adaptation | JAR-6 |
| [g711-resample.md](g711-resample.md) | PCMU, PCMA, one resample stream | JAR-5, JAR-6 |
| [dtmf.md](dtmf.md) | telephone-event, `ReadDTMF`, `WriteDTMF` | JAR-7 |
| [rtcp.md](rtcp.md) | Compound SR/RR and the RTCP latch | JAR-5 |
| [inbound-dialog.md](inbound-dialog.md) | INVITE through ACK, digest, CANCEL, BYE, `Accept` | JAR-8 |
| [outbound-dialog.md](outbound-dialog.md) | `Dial`, digest retry, CANCEL | JAR-9 |
| [reinvite.md](reinvite.md) | re-INVITE, hold direction, one retry | JAR-8 |
| [update.md](update.md) | RFC 3311 UPDATE | JAR-8 |
| [100rel.md](100rel.md) | PRACK, reliability on our dialog | JAR-8 |
| [refer.md](refer.md) | Blind and attended REFER, NOTIFY | JAR-9 |
| [replaces.md](replaces.md) | Replaces on REFER and on INVITE, `OnReplaced` | JAR-9 |
| [early-media.md](early-media.md) | 183 with SDP played into the pipeline | JAR-7 |
| [register.md](register.md) | REGISTER of `sip:user@domain` | JAR-11 |
| [session-timers.md](session-timers.md) | RFC 4028 refresh and expiry | JAR-11 |
| [options.md](options.md) | OPTIONS 200, not a media operation | JAR-8, JAR-11 |
| [pipeline-adapter.md](pipeline-adapter.md) | `NewTransport`, native DTMF, BYE, resample edge | JAR-10 |
| [custom-headers.md](custom-headers.md) | Read and set caller headers on every message | JAR-13 |

## Public functions

| Function | Page |
| --- | --- |
| `New`, `Start`, `Close` | [config.md](config.md) |
| `Accept` | [inbound-dialog.md](inbound-dialog.md) |
| `Dial` | [outbound-dialog.md](outbound-dialog.md) |
| `OnReplaced` | [replaces.md](replaces.md) |
| `ReadPCM`, `WritePCM` | [g711-resample.md](g711-resample.md), [jitter.md](jitter.md) |
| `ReadDTMF`, `WriteDTMF` | [dtmf.md](dtmf.md) |
| `Hold`, `Resume` | [reinvite.md](reinvite.md), [update.md](update.md) |
| `Transfer`, `TransferAttended` | [refer.md](refer.md) |
| `Hangup`, `Remote`, `Done` | [config.md](config.md) |
| `SetHeader`, `Header` | [custom-headers.md](custom-headers.md) |
| `NewTransport` | [pipeline-adapter.md](pipeline-adapter.md) |

## Pins

Releases on or before 2026-11-15. Versions, times, and commits are what `proxy.golang.org` returned. Checksums are recorded when `go.sum` is written, from the checksum database, and are not listed here.

| Module | Version | Time (UTC) | Commit | Source |
| --- | --- | --- | --- | --- |
| `github.com/emiago/sipgo` | v1.7.3 | 2026-11-10T22:07:24Z | `c6c20f2df8a7d8bfae61b8a1901fec78bf9bcb34` | [info](https://proxy.golang.org/github.com/emiago/sipgo/@v/v1.7.3.info) |
| `github.com/icholy/digest` | v1.2.0 | 2026-07-28T16:25:21Z | `104e41263110fbc4690a1c0d6b309cac6e597e56` | [info](https://proxy.golang.org/github.com/icholy/digest/@v/v1.2.0.info). sipgo v1.7.3 requires this version. |
| `github.com/pion/rtp` | v1.10.6 | 2026-10-20T23:57:06Z | `9a8f93554133824676326b43f068167fef01a884` | [info](https://proxy.golang.org/github.com/pion/rtp/@v/v1.10.6.info) |
| `github.com/pion/rtcp` | v1.2.20 | 2026-10-21T04:56:30Z | `cd21e4757f92fce4a88ad7976baeec9b7690fb6a` | [info](https://proxy.golang.org/github.com/pion/rtcp/@v/v1.2.20.info) |
| `github.com/pion/stun/v4` | v4.0.1 | 2026-09-15T10:05:46Z | `45d610324d90b26e95024a7febcaa86fdc7f83d7` | [info](https://proxy.golang.org/github.com/pion/stun/v4/@v/v4.0.1.info) |
| `github.com/pion/sdp/v3` | v3.0.20 | 2026-09-15T20:57:46Z | `8921edc83d51945017b2e1a987733a23bb9a9004` | [info](https://proxy.golang.org/github.com/pion/sdp/v3/@v/v3.0.20.info) |
| `github.com/gojargo/jargo` | v0.6.1 | 2026-11-12T21:48:37Z | `7d0492d2c0febfaa0c140007317992f84c8c1be9` | [info](https://proxy.golang.org/github.com/gojargo/jargo/@v/v0.6.1.info) |

Use these tags. `audio/g711` on v0.6.1 returns `[]byte` from `EncodeULaw`, `DecodeULaw`, `EncodeALaw`, and `DecodeALaw`. A later main of jargo changed that signature; this build follows the tag.

jargo v0.6.1 is the module at `github.com/gojargo/jargo`. The engine stays free of `frames`, `processor`, and `pipeline`. The adapter is the file that imports those. Codec and resample code may import `audio`, `audio/g711`, and `audio/resample` from that module. Do not fork those packages. Do not import diago. Do not edit `transport/rtc`, `transport/livekit`, or `transport/wsserver` (they live in jargo). Do not add a dev-runner SIP transport.

## Out of scope

WebRTC bridge, SRTP, ICE, DTLS, video, Opus on the SIP leg, comfort noise payload 13, T.38, a SIP proxy, and a registrar server. `a=rtcp-mux` is absent. Pion's interceptor jitter buffer is unused.

## Build order

Each story is tested before the next. Acceptance tests in the plan are the definition of done for the story that owns them.

1. JAR-2 specs (this tree).
2. JAR-3 config, bind, both NAT modes. STUN refused behind an SBC. Each RTP socket does its own Binding.
3. JAR-4 SDP and the port allocator. Round-trip our offer and a SIPp offer. Blocked on JAR-3.
4. JAR-5 media session. Two localhost sessions. Latch from a non-SDP port. RTCP SR on the paired port. G.711 matches `audio/g711` vectors. Blocked on JAR-4.
5. JAR-6 jitter. Target 60 ms does not start early. A dropped packet is one concealment frame. 8000 builds no resampler. 320-byte PCM becomes one 160-byte payload. 16000 builds one resampler and the payload is still 160 bytes. Blocked on JAR-5.
6. JAR-7 DTMF and 183 early media. Blocked on JAR-6.
7. JAR-8 inbound dialogs, digest, CANCEL, BYE, re-INVITE, UPDATE, OPTIONS, 100rel. Blocked on JAR-5 so media can move.
8. JAR-9 outbound, blind REFER, attended REFER, Replaces. Blocked on JAR-8.
9. JAR-10 `NewTransport`. Blocked on JAR-7 and JAR-9.
10. JAR-11 REGISTER, session timers, OPTIONS against a test registrar. Blocked on JAR-8.
11. JAR-13 custom headers on every message. Blocked on JAR-11.

## Shared acceptance

- SIPp UAC: 5 s of PCMU, telephone-event 5 with three end packets, the digit once, `Done` closes. A run that never sets the end bit still returns 5 once when the next timestamp starts. Owner: JAR-7.
- SIPp UAS: SDP `c=` is the local IP in `behind-sbc` and the advertise IP in `standalone`. Owner: JAR-3 and JAR-4.
- A spoofed packet after commit leaves the latch where it is. Owner: JAR-5.
- re-INVITE `sendonly` stops `ReadPCM` caller audio. The sender continues silence with the sequence unbroken. Owner: JAR-8.
- UPDATE hold does the same with no ACK. UPDATE that changes the RTP port resets the latch. Owner: JAR-8.
- Blind REFER ends with NOTIFY 200, BYE on the original, and a live call on the new one. Attended REFER carries Replaces. Owner: JAR-9.
- Crossing re-INVITEs: 491 and exactly one retry completes. Owner: JAR-8.
- A 401 is answered with digest exactly once. Owner: JAR-8 inbound, JAR-9 outbound.
