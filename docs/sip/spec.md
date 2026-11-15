# Jargo SIP endpoint library

Amendment: this file wins over any older spec. Repo: github.com/smlsr/jargo-sip. The checkout on the coordinator is empty except README, LICENSE, and .gitignore. Implement the library here.

Package path: `transport/sip` inside the module. No binary. Do not import diago. Signaling is sipgo (`github.com/emiago/sipgo`) for transactions and dialogs only. Media is ours on `pion/rtp` and `pion/rtcp`, using G.711. sipgo `DialogServerCache` and `DialogClientCache` are examples; the call table lives on `Endpoint`.

Public surface: `Endpoint` (one process, one SIP port, many calls) and `Call` (one dialog, one RTP session, PCM in and out). `Transport` wraps one `Call` the way `transport/rtc` wraps one PeerConnection.

jargo types (`transport.BaseInput`, `BaseOutput`, `audio/g711`, `audio/resample`) may not be in this repo. Find the real jargo module the user owns before inventing a fork. If it cannot be imported, keep the engine free of jargo and put the adapter behind the same method names the plan names. Do not edit `transport/rtc`, `transport/livekit`, or `transport/wsserver`. Do not add a dev-runner SIP transport. Out of scope: WebRTC bridge, SRTP, video, Opus on the SIP leg, CN payload 13, T.38, proxy, registrar.

Pin dependencies to releases dated on or before 2026-11-15. Record version, date, and source. Never invent a version, measurement, or checksum.

## Address modes

`NATMode`: `behind-sbc` or `standalone`. The bind socket is always local. Only advertised addresses change.

behind-sbc: SBC is on the same L3 network. Via, Contact, SDP `c=`, and SDP `m=` use the local IP and port. No STUN. A configured public AdvertiseIP is a configuration error and `Endpoint.Start` refuses it. PortMap is forced to `identity`.

standalone: bind local, advertise public. AdvertiseIP order: Config.AdvertiseIP if set, else STUN Binding (RFC 8489) from the SIP UDP socket to Config.STUNServer. Mapped IP is the advertise IP. Mapped port is recorded separately.

PortMap: `identity` uses the local bind port. `stun` uses that socket's mapped port. Each RTP socket does its own Binding request. One STUN result is not reused across calls. SIP and RTP are different sockets.

`Config.Domain` is the SIP realm and AoR host, not the Contact host. From outbound: `"Bot" <sip:bot@domain>` (display name and user from config). Contact: `<sip:user@advertiseIP:sipPort;transport=udp>`. Via sent-by: advertiseIP:sipPort. REGISTER, when enabled, registers `sip:user@domain` and digests against that realm. Direct trunks leave REGISTER off and match INVITE source against AllowFrom.

## Config

```go
type NATMode string // "behind-sbc" | "standalone"

type TLSConfig struct {
    CertFile           string // with KeyFile, the listener certificate
    KeyFile            string
    SelfSigned         bool // generate when CertFile and KeyFile are empty
    IgnoreCertWarnings bool // outbound TLS accepts a peer certificate that fails verification
}

type Config struct {
    NATMode      NATMode
    ListenIP     string
    AdvertiseIP  string
    STUNServer   string
    PortMap      string // "identity" | "stun"
    Domain       string
    User         string
    DisplayName  string
    SIPPort      int // default 5060
    Transports   []string // udp, tcp, tls. Empty means udp
    TLS          *TLSConfig // required when tls is listed
    RTPPortMin   int // default 10000
    RTPPortMax   int // default 20000, even span
    Auth         *DigestAuth
    RequireAuth  bool
    AllowFrom    []string // empty refuses every INVITE
    Register     *RegisterConfig // nil: do not register
    Jitter       JitterConfig
    Codecs       []Codec // default PCMU, PCMA
    DTMF         DTMFConfig
    Latch        LatchConfig
    RTCP         RTCPConfig
    SessionTimer time.Duration // 0 disables RFC 4028
    MaxCall      time.Duration
}
```

Jitter defaults: Target 40ms, Min 20ms, Max 200ms. DTMF: PayloadType 0 means 101; DigitDuration default 160ms; InterDigitGap default 100ms. Inbound offer payload type wins.

Start refuses: behind-sbc combined with STUN or a public AdvertiseIP, empty Domain, RTP range odd or inverted, AllowFrom empty. `tls` in Transports listens on the SIP port. `TLS.CertFile` and `TLS.KeyFile`, when both are set, are the listener certificate and key. When `SelfSigned` is set and no certificate files are given, Start generates a self-signed certificate for the advertised address and uses it on the TLS listener. `IgnoreCertWarnings`, when set, makes outbound TLS accept a peer certificate that fails verification, including a self-signed peer. When it is unset, verification stays on. Empty Transports stays UDP. `tcp` is unchanged. UDP and TLS can both be enabled.

API:

```go
func New(cfg Config) (*Endpoint, error)
func (e *Endpoint) Start(ctx context.Context) error
func (e *Endpoint) Accept() (*Call, error)
func (e *Endpoint) Dial(ctx context.Context, uri string) (*Call, error)
func (e *Endpoint) OnReplaced(func(old, new *Call))
func (e *Endpoint) Close() error

func (c *Call) ReadPCM() ([]byte, error) // 20ms, 16-bit LE, negotiated clock
func (c *Call) WritePCM(pcm []byte) error
func (c *Call) ReadDTMF() (DTMF, error)
func (c *Call) WriteDTMF(ev DTMF) error
func (c *Call) Hold(ctx context.Context) error
func (c *Call) Resume(ctx context.Context) error
func (c *Call) Transfer(ctx context.Context, target string) error
func (c *Call) TransferAttended(ctx context.Context, other *Call) error
func (c *Call) Hangup(ctx context.Context) error
func (c *Call) Remote() Party
func (c *Call) Done() <-chan struct{}
func NewTransport(c *Call, p transport.Params) *Transport
```

ReadPCM returns a concealment frame on underrun, not an error. WritePCM is buffered; the sender owns the 20ms clock.

## Sample rate

PCMU and PCMA stay at 8000. ReadPCM returns 160 samples (320 bytes). No resampler is built when the pipeline rate is 8000. Wire clock is the negotiated codec clock. Pipeline rate is the transport param, default 8000 in and 8000 out. Convert once with one audio/resample stream only when they differ. No waypoint at 16000 or 24000. ResamplerClearAfter stays negative. A negotiated clock above the pipeline is downsampled. Equal clocks are a copy.

## Call setup

Inbound: OnInvite, drop unless AllowFrom matches, 100 Trying. RequireAuth challenges 401. sipgo does not do this. 488 if neither PCMU nor PCMA. Choose the first of our Codecs that the offer lists. Take telephone-event/8000 at the offer's dynamic type. Missing telephone-event still connects; send returns ErrDTMFNotNegotiated. Allocate even RTP port, bind RTP and RTP+1. standalone PortMap stun: Binding-request both sockets before SDP. 200 OK with SDP. OnAck starts media against the offer RTP address and Accept returns the Call. Until latch commits, send to the SDP address. CANCEL before 200 answers 487 and frees sockets. BYE after establishment answers 200 and closes the Call.

Outbound: bind RTP and RTCP, STUN if required. INVITE via sipgo dialog client. SDP offer: PCMU, PCMA, telephone-event/8000 payload 101, `a=fmtp:101 0-15`, `a=ptime:20`, `a=sendrecv`. The answer's payload type is what we send and accept. One digest retry on 401 or 407. A second challenge fails the dial. 183 with SDP: remember the address and play early media into the pipeline. This build supports 183 early media. The older "do not play it" line loses. On 200, parse, ACK, start media. Transport error or timeout before 200 sends CANCEL.

re-INVITE and UPDATE are both in the first build. Every response carries `Allow: INVITE, ACK, CANCEL, BYE, UPDATE, PRACK, REFER, NOTIFY, OPTIONS`. One offer/answer lock. CSeq is per direction. A request we send is one higher than the last request we sent. A request we receive updates only the remote sequence. ACK and CANCEL copy the INVITE CSeq and do not increment. An INVITE that arrives while an INVITE we sent is unanswered is 491. A second INVITE that arrives before we have sent the final response to an INVITE we received is 500 with Retry-After chosen at random from 0 to 10 seconds. Crossing re-INVITEs are the 491 case. On a 491, retry once. The UA that owns the Call-ID waits 2.1–4s. The other UA waits 0–2s (RFC 3261 §14.1, the timer §14.2 uses).

re-INVITE: same Call-ID, tags. The re-INVITE CSeq is one higher than the last request we sent. With SDP, answer in the 200 and commit the media change when that 200 is sent. The first INVITE still starts media on ACK. `o=` version increments on every offer we send. Without SDP, 200 with current SDP. Store Contact. Media moves only if `c=` or `m=` changed. sendonly: stop playout, keep sending silence. recvonly: send silence, keep playing. inactive: both. sendrecv restores. Hold and Resume prefer UPDATE when allowed, else re-INVITE. New address resets latch to SDPTarget. SSRC, sequence, and local sockets stay. Codec we offered applies on the next sender tick. Codec we did not offer is 488 and the previous session stays.

UPDATE (RFC 3311): answer is the response, no ACK. Legal in early and confirmed dialogs. Do not send UPDATE while an INVITE offer is open; queue until ACK. An offer while we generated an offer that is still unanswered is 491. An offer while we received an offer we have not answered is 500 with Retry-After random from 0 to 10 seconds. An UPDATE before we have sent a final response to a previous UPDATE is the same 500. No SDP: 200, media untouched. With SDP: same direction, latch, and codec rules.

Session timers (RFC 4028) when SessionTimer is set: refresh at half the interval via UPDATE if allowed, else re-INVITE. Expiry hangs up. Advertise Session-Expires and Supported: timer on the original INVITE.

OPTIONS: 200 with the same Allow. Not a media operation.

100rel and PRACK are in the first build. The INVITE carries Supported: 100rel, not Require. Send Require: 100rel and RSeq, and retransmit, only for a 1xx on an INVITE that offered or required 100rel. 100 stays unreliable. An INVITE with Require: 100rel and no 18x can still end in 200. Reliability state stays on our dialog. Confirm sipgo types while implementing. Do not fork sipgo.

## Transfer

Blind REFER (RFC 3515): Refer-To target, no Replaces. Expect 202, then NOTIFY message/sipfrag. SIP/2.0 200 means BYE the original dialog. 4xx/5xx sipfrag or no NOTIFY before the subscription expires leaves the original call up and returns the failure.

Attended: other Call must be established. Refer-To is that call's remote target with embedded Replaces. Tags as seen by the party who receives the new INVITE. Referred-By (RFC 3892). Same NOTIFY watch and BYE-on-success.

Inbound REFER: 202, NOTIFY 100 Trying immediately, then further sipfrags. Subscription-State active, then terminated;reason=noresource. Dial Refer-To. If it contains Replaces, the outbound INVITE carries a Replaces header. On 200, NOTIFY 200, BYE original, hand the app the new Call via OnReplaced(old, new) before the old call's client-disconnected event. Failure: NOTIFY that status, leave the original up, do not fire OnReplaced. Refer-To method other than INVITE, or an unparseable nested URI: 400, not 202.

INVITE with Replaces: we are the target. Answer as a normal call, BYE the named dialog, fire OnReplaced. Unknown or ending dialog: 481. Early dialog: 491 unless early-only and the dialog is still early, then answer the new one and 487 the old one.

## SDP we send

```
v=0
o=- <sess> <vers> IN IP4 <advertiseIP>
s=-
c=IN IP4 <advertiseIP>
t=0 0
m=audio <rtpPort> RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-15
a=ptime:20
a=sendrecv
```

No a=rtcp-mux. RTCP is RTP port + 1. Add `a=rtcp:<port>` only when the two ports are not consecutive (PortMap stun). `o=` session id is random per call; version increments per offer (RFC 3264). No SRTP, ICE, DTLS, Opus, video, T.38, CN 13.

## Latch

States: SDPTarget, Provisional, Committed. On establish, send RTP and RTCP to the SDP address. First plausible matched RTP moves to Provisional and the sender retargets immediately. That audio enters the jitter buffer. Commit on 3 packets from that 5-tuple, or 100ms with no contradicting source. Inside 500ms of the first packet, a different source with more plausible packets switches once. After commit the address is frozen. Further sources are logged and dropped. re-INVITE or UPDATE with a new c= or m= returns to SDPTarget. Do not rebind. Only the WriteTo address changes.

Plausible RTP: version 2; payload type is negotiated audio or telephone-event; SSRC unseen or the accepted SSRC (a second SSRC is dropped); audio payload 160 bytes, or 80 accepted as 10ms; not STUN (magic cookie 0x2112A442 at byte 4).

LatchConfig.Match: behind-sbc default `port` (source IP equals SDP c= or AllowRTPFrom; port may differ). standalone default `address` (IP and port from the packet). `off` never leaves SDPTarget.

RTCP latch is independent. Until commit, send to a=rtcp if present, else RTP port + 1 on the SDP connection address. First valid compound (version 2, first packet type 200–204) is provisional. Commit is 2 packets or 100ms. RTCP BYE is logged and surfaced. It does not hang up. SIP BYE is the only hang-up.

Sender starts at establishment with silence (µ-law 0xFF or A-law 0xD5), not pipeline silence. Pipeline AudioOutFillsSilence stays on.

## RTP sender

One goroutine, 20ms ticker, monotonic clock. Late ticks send one packet, not a burst. SSRC random and constant. Initial sequence and timestamp random (RFC 3550). Timestamp advances by the sample count, including silence. Marker 1 on the first packet of a talkspurt.

## Jitter buffer

Our sequence ring. Do not use pion's interceptor jitter buffer. Target is how long the playout head waits behind the newest packet. Playout ticker 20ms. Start after Target of audio is buffered, or the oldest packet is older than Target. Gap: repeat the previous frame once, count an underrun, do not wait. Telephone-event is demuxed before decode, consumes the sequence, and is not loss. The playout tick conceals that 20ms. Late packets are dropped. Every 2s, if depth sat above Target+20ms with no underrun, lower Target by 20ms to Min. On underrun, raise Target by 20ms to Max. Reset the buffer when remote address changes.

## DTMF

Offer 101 and fmtp 0-15. Accept the offer's dynamic type at 8000 only. Missing fmtp means 0-15. Other clocks ignored. No in-band tones. Events 0-9, 10 star, 11 hash, 12-15 A-D, 16 flash. A-D and flash stay on Call and are not keypad frames.

Receive: one DTMF when E is first seen for (SSRC, timestamp, event), or when a new timestamp starts and the previous press never set E. Retransmitted ends do not emit again. Marker not required. Event outside fmtp is dropped.

Send: event replaces audio. Timestamp stays at the start. Every 20ms sequence increments, duration grows by 160, E clear. Default 160ms. Then three end packets with E set, sequence still incrementing. Next audio timestamp is start + final duration. InterDigitGap silence between keys. OutputDTMFUrgentFrame cuts in on the next tick. Volume sent as 10.

## RTCP

pion/rtcp. Compound packets. Separate socket. Every 5s and when there is something to report: SR if we have sent, plus RR if we have received, plus SDES CNAME `user@domain`. RR only before we have sent. Fraction and cumulative lost from the jitter buffer. Jitter is RFC 3550 interarrival. LSR and DLSR from the last SR. RTT is logged, not applied to the jitter target. Payload type outside 200-204 dropped. RTP on the RTCP socket logged once per call. RTCP ticker must not stall audio.

## Adapter

SupportsNativeDTMF true. WriteDTMFNative. Keys are not mixed into audio. AudioOut10msChunks must be 2. Keypad 0-9, star, hash map to events 0-11. EndFrame and CancelFrame send BYE. Remote BYE closes the Call and the pipeline sees client disconnect. OnReplaced fires before that disconnect.

## Build order and tests

Each step tested before the next. Stories and blockers:

1. JAR-2 specs. Docs only.
2. JAR-3 config, bind, address resolution. Both NAT modes. STUN refused behind an SBC. Each RTP socket gets its own Binding request.
3. JAR-4 SDP and port allocator. Round-trip our offer and a SIPp offer. No network yet. Blocked on JAR-3.
4. JAR-5 media session. Two localhost sessions. Latch from a non-SDP port. RTCP SR on the paired port. G.711 matches audio/g711 vectors. Blocked on JAR-4.
5. JAR-6 jitter. Target 60ms does not start early. A dropped packet is one concealment frame. 8000 builds no resampler. 320-byte PCM becomes one 160-byte payload. 16000 builds one resampler and the payload is still 160 bytes. Blocked on JAR-5.
6. JAR-7 DTMF and 183 early media. Blocked on JAR-6.
7. JAR-8 inbound dialogs, digest, CANCEL, BYE, re-INVITE, UPDATE, OPTIONS, 100rel. Blocked on JAR-5 so media can move.
8. JAR-9 outbound, blind REFER, attended REFER, Replaces. Blocked on JAR-8.
9. JAR-10 NewTransport pipeline. Blocked on JAR-7 and JAR-9.
10. JAR-11 REGISTER, session timers, OPTIONS against a test registrar. Blocked on JAR-8.
11. JAR-13 custom headers on every message. Blocked on JAR-11.

## Custom headers

Every request and response can carry caller-supplied headers, and every received message exposes the headers that arrived. Names are compared case-insensitively. Hop-by-hop headers we own (Via, Contact, CSeq, Call-ID, From, To, Max-Forwards, Content-Length, Route, Record-Route) are not replaced by a custom value. A custom header set on Dial is on the INVITE and copied onto its ACK and CANCEL. A custom header set on Transfer or TransferAttended is on the REFER. Inbound INVITE, UPDATE, re-INVITE, NOTIFY, and responses expose the received headers on the Call before the app acts. The same read and set path covers every other method, including BYE, OPTIONS, REGISTER, PRACK, and UPDATE.

Acceptance tests from the plan are the definition of done for the story that owns them. SIPp UAC: 5s PCMU, telephone-event 5 with three end packets, digit once, Done closes. A run that never sets E still returns 5 once when the next timestamp starts. SIPp UAS: SDP c= is local IP behind-sbc and advertise IP standalone. Spoofed packet after commit does not move the latch. re-INVITE sendonly stops ReadPCM caller audio and the sender continues silence with sequence unbroken. UPDATE hold does the same with no ACK. UPDATE that changes the RTP port resets the latch. Blind REFER ends with NOTIFY 200, BYE on the original, live call on the new one. Attended REFER carries Replaces. Crossing re-INVITEs: 491 and exactly one retry completes. 401 answered with digest exactly once.
