# Config and endpoint API

Owner: JAR-3. Package `github.com/smlsr/jargo-sip/transport/sip`.

`New` validates what it can without a socket. `Start` binds, resolves the advertise address ([nat-stun.md](nat-stun.md)), and then accepts calls. `Close` hangs up every call, closes the SIP and RTP sockets, and unblocks `Accept`, `ReadPCM`, and `ReadDTMF`.

## Types

```go
type NATMode string

const (
    NATBehindSBC  NATMode = "behind-sbc"
    NATStandalone NATMode = "standalone"
)

type Codec string

const (
    CodecPCMU Codec = "PCMU" // payload 0, 8000 Hz
    CodecPCMA Codec = "PCMA" // payload 8, 8000 Hz
)

type DigestAuth struct {
    Username string // empty uses Config.User
    Password string
}

// RegisterConfig enables REGISTER. Nil leaves registration off.
// See register.md.
type RegisterConfig struct {
    Registrar string        // empty uses sip:Domain
    Expires   time.Duration // zero omits Expires and keeps the registrar's choice
}

type JitterConfig struct {
    Target time.Duration // zero means 40ms
    Min    time.Duration // zero means 20ms
    Max    time.Duration // zero means 200ms
}

type DTMFConfig struct {
    PayloadType   int           // zero means 101 on offers we send
    DigitDuration time.Duration // zero means 160ms
    InterDigitGap time.Duration // zero means 100ms
}

type LatchConfig struct {
    Match        string   // "", "port", "address", "off". See latch.md.
    AllowRTPFrom []string // extra source IPs for Match "port"
}

type RTCPConfig struct {
    Interval time.Duration // zero means 5s. See rtcp.md.
}

// TLSConfig is the SIP TLS listener certificate and the outbound verification policy.
// Required when Transports contains tls.
type TLSConfig struct {
    CertFile           string // with KeyFile, used as given
    KeyFile            string
    SelfSigned         bool // generate when CertFile and KeyFile are empty
    IgnoreCertWarnings bool // outbound TLS accepts a peer certificate that fails verification
}

type Config struct {
    NATMode      NATMode
    ListenIP     string // empty binds 0.0.0.0. IPv4 only.
    AdvertiseIP  string
    STUNServer   string // host:port, RFC 8489
    PortMap      string // "identity" or "stun". Empty follows the NAT mode.
    Domain       string // SIP realm and AoR host. Not the Contact host.
    User         string
    DisplayName  string
    SIPPort      int      // zero means 5060
    Transports   []string // empty means udp. See below.
    TLS          *TLSConfig
    RTPPortMin   int      // zero means 10000 when RTPPortMax is also zero
    RTPPortMax   int      // zero means 20000 when RTPPortMin is also zero
    Auth         *DigestAuth
    RequireAuth  bool
    AllowFrom    []string // IP or CIDR. Empty is refused.
    Register     *RegisterConfig
    Jitter       JitterConfig
    Codecs       []Codec // empty means PCMU then PCMA
    DTMF         DTMFConfig
    Latch        LatchConfig
    RTCP         RTCPConfig
    SessionTimer time.Duration // zero disables RFC 4028
    MaxCall      time.Duration // zero means no cap
}
```

`Transports`: an empty list means UDP. `udp` and `tcp` listen on `SIPPort`. `tls` listens on `SIPPort` when `TLS` supplies a certificate. UDP and TLS can both be enabled. RTP stays UDP. STUN still runs on the SIP UDP socket.

`TLS` is required when `tls` is listed. `CertFile` and `KeyFile`, when both are set, are the listener certificate and private key, including when `SelfSigned` is also set. When `SelfSigned` is set and those files are empty, `Start` generates a self-signed certificate for the advertised address and uses it on the TLS listener. `IgnoreCertWarnings`, when set, makes outbound TLS accept a peer certificate that fails verification, including a self-signed peer. When it is unset, verification stays on. A TLS-only listener puts `transport=tls` on Contact. When UDP is enabled, Contact stays `transport=udp`.

`AllowFrom` entries are IP addresses or CIDR prefixes. The match is the source address of the SIP packet, not the Via host. A name is a `Start` error.

`User` empty is a `Start` error. From, Contact, and the AoR all use it.

`MaxCall` greater than zero hangs the call up at that age from answer. The hang-up is a SIP BYE.

## Identity headers

From on outbound requests:

```text
"DisplayName" <sip:User@Domain>
```

An empty `DisplayName` omits the display name. Contact:

```text
<sip:User@advertiseIP:sipPort;transport=udp>
```

Via sent-by is `advertiseIP:sipPort`. `advertiseIP` and `sipPort` come from [nat-stun.md](nat-stun.md). `Domain` is the realm and the host of `sip:User@Domain`. It is not the Contact host.

Every response we send carries:

```text
Allow: INVITE, ACK, CANCEL, BYE, UPDATE, PRACK, REFER, NOTIFY, OPTIONS
```

`Supported` includes `100rel`. It includes `timer` when `SessionTimer` is set. See [100rel.md](100rel.md) and [session-timers.md](session-timers.md).

## Start refusals

`Start` returns an error, and binds nothing, when any of these hold:

- `NATMode` is neither `behind-sbc` nor `standalone`.
- `behind-sbc` combined with STUN or an advertise address: `STUNServer` non-empty, `AdvertiseIP` non-empty, or `PortMap` equal to `stun`.
- `standalone` with `AdvertiseIP` empty and `STUNServer` empty.
- `Domain` empty, or `User` empty.
- `AllowFrom` empty, or an entry that is not an IP or CIDR.
- The RTP range is odd or inverted after defaults are applied. Both ends must be even and `RTPPortMin` must be less than or equal to `RTPPortMax`. A range with no even port is refused. See [ports.md](ports.md).
- `ListenIP` is not empty and not an IPv4 address.
- `Transports` names an unknown transport, or names `tls` without a certificate. A certificate is `CertFile` and `KeyFile` together, or `SelfSigned` with neither file. One file without the other is refused.
- `Codecs` contains anything other than `PCMU` or `PCMA`.
- `Latch.Match` is set and is not `port`, `address`, or `off`.
- `PortMap` is set and is not `identity` or `stun`.
- `RequireAuth` is set and `Auth` is nil or `Auth.Password` is empty.

`behind-sbc` forces `PortMap` to `identity`.

## Call API

```go
func New(cfg Config) (*Endpoint, error)
func (e *Endpoint) Start(ctx context.Context) error
func (e *Endpoint) Accept() (*Call, error)
func (e *Endpoint) Dial(ctx context.Context, uri string, headers ...Header) (*Call, error)
func (e *Endpoint) OnReplaced(func(old, new *Call))
func (e *Endpoint) Close() error

func (c *Call) ReadPCM() ([]byte, error)  // 20ms, 16-bit LE, negotiated clock
func (c *Call) WritePCM(pcm []byte) error // buffered; the sender owns the 20ms clock
func (c *Call) ReadDTMF() (DTMF, error)
func (c *Call) WriteDTMF(ev DTMF) error
func (c *Call) Hold(ctx context.Context) error
func (c *Call) Resume(ctx context.Context) error
func (c *Call) Transfer(ctx context.Context, target string, headers ...Header) error
func (c *Call) TransferAttended(ctx context.Context, other *Call, headers ...Header) error
func (c *Call) SetHeader(name, value string)
func (c *Call) Header(name string) (string, bool)
func (e *Endpoint) SetHeader(name, value string)
func (e *Endpoint) Header(name string) (string, bool)
func (c *Call) Hangup(ctx context.Context) error
func (c *Call) Remote() Party
func (c *Call) Done() <-chan struct{}

type Party struct {
    URI          string
    DisplayName  string
    CallID       string
    LocalTag     string
    RemoteTag    string
    RemoteTarget string // Contact URI of the peer
}

type DTMF struct {
    Event    uint8         // 0-16, see dtmf.md
    Duration time.Duration // zero on send uses DTMFConfig.DigitDuration
}
```

`ReadPCM` returns 160 samples, 320 bytes, at 8000 Hz. PCMU and PCMA are both 8000. On underrun it returns one concealment frame and a nil error. After `Done` is closed it returns an error.

`WritePCM` queues 8000 Hz PCM. The RTP sender pulls 20 ms at a time. A short buffer waits. After `Done` is closed, `WritePCM` returns an error.

`Hangup` sends BYE on an established dialog and CANCEL on an outbound dialog that has not reached 200. `Done` closes after BYE completes, CANCEL completes, the peer's BYE, `MaxCall`, session-timer expiry, or `Endpoint.Close`.

`Header` values on `Dial`, `Transfer`, and `TransferAttended`, and `SetHeader` / `Header`, are specified in [custom-headers.md](custom-headers.md).

`Remote` returns the peer as currently stored, including the remote target used by attended transfer.

`ErrDTMFNotNegotiated` is the error from `WriteDTMF` when telephone-event was not negotiated. The call stays up.

The call table is on `Endpoint`, keyed by dialog id. sipgo `DialogServerCache` and `DialogClientCache` are not that table.
