package sip

import "time"

// NATMode selects how an endpoint advertises itself.
// The bind socket is always local.
type NATMode string

const (
	// NATBehindSBC means the SBC is on the same L3 network.
	// Via, Contact, and the media address use the local IP and port.
	NATBehindSBC NATMode = "behind-sbc"
	// NATStandalone binds locally and advertises a public address.
	NATStandalone NATMode = "standalone"
)

const (
	// PortMapIdentity advertises the local bind port.
	PortMapIdentity = "identity"
	// PortMapSTUN advertises the port mapped for that socket.
	PortMapSTUN = "stun"

	LatchMatchPort    = "port"
	LatchMatchAddress = "address"
	LatchMatchOff     = "off"
)

// Codec is a voiced codec name. PCMU and PCMA are the defaults.
type Codec string

const (
	CodecPCMU Codec = "PCMU"
	CodecPCMA Codec = "PCMA"
)

// DigestAuth holds credentials for a digest challenge.
// The realm is Config.Domain.
type DigestAuth struct {
	Username string
	Password string
}

// RegisterConfig enables REGISTER when it is non-nil.
// An empty Registrar uses Domain.
type RegisterConfig struct {
	Registrar string
	Expires   time.Duration
}

// JitterConfig is the playout delay. Zero values become 40 ms, 20 ms, and
// 200 ms. Start rounds each value up to a 20 ms frame and keeps
// Min <= Target <= Max.
type JitterConfig struct {
	Target time.Duration
	Min    time.Duration
	Max    time.Duration
}

// DTMFConfig is stored for telephone-event. PayloadType 0 means 101.
type DTMFConfig struct {
	PayloadType   int
	DigitDuration time.Duration
	InterDigitGap time.Duration
}

// LatchConfig selects how a source is matched. Empty uses the NAT mode default.
// AllowRTPFrom lists extra source IPs accepted when Match is port.
type LatchConfig struct {
	Match        string
	AllowRTPFrom []string
}

// RTCPConfig is the report interval. Zero becomes 5s.
type RTCPConfig struct {
	Interval time.Duration
}

// Config is the endpoint configuration. Start validates it and binds.
type Config struct {
	NATMode      NATMode
	ListenIP     string
	AdvertiseIP  string
	STUNServer   string
	PortMap      string // "identity" or "stun"
	Domain       string
	User         string
	DisplayName  string
	SIPPort      int // default 5060
	Transports   []string
	RTPPortMin   int // default 10000
	RTPPortMax   int // default 20000, even span
	Auth         *DigestAuth
	RequireAuth  bool
	AllowFrom    []string
	Register     *RegisterConfig
	Jitter       JitterConfig
	Codecs       []Codec
	DTMF         DTMFConfig
	Latch        LatchConfig
	RTCP         RTCPConfig
	SessionTimer time.Duration
	MaxCall      time.Duration
}

func applyDefaults(cfg Config) Config {
	if cfg.SIPPort == 0 {
		cfg.SIPPort = 5060
	}
	if cfg.RTPPortMin == 0 {
		cfg.RTPPortMin = 10000
	}
	if cfg.RTPPortMax == 0 {
		cfg.RTPPortMax = 20000
	}
	if cfg.PortMap == "" {
		cfg.PortMap = PortMapIdentity
	}
	cfg.Jitter = normalizeJitter(cfg.Jitter)
	if cfg.DTMF.PayloadType == 0 {
		cfg.DTMF.PayloadType = 101
	}
	if cfg.DTMF.DigitDuration == 0 {
		cfg.DTMF.DigitDuration = 160 * time.Millisecond
	}
	if cfg.DTMF.InterDigitGap == 0 {
		cfg.DTMF.InterDigitGap = 100 * time.Millisecond
	}
	if len(cfg.Codecs) == 0 {
		cfg.Codecs = []Codec{CodecPCMU, CodecPCMA}
	}
	if cfg.Latch.Match == "" {
		switch cfg.NATMode {
		case NATBehindSBC:
			cfg.Latch.Match = LatchMatchPort
		case NATStandalone:
			cfg.Latch.Match = LatchMatchAddress
		}
	}
	if cfg.RTCP.Interval == 0 {
		cfg.RTCP.Interval = 5 * time.Second
	}
	return cfg
}

// normalizeJitter fills zeros and rounds up to the 20 ms playout frame.
// Min is then lowered so it does not exceed Target, and Target is lowered
// so it does not exceed Max.
func normalizeJitter(j JitterConfig) JitterConfig {
	if j.Target <= 0 {
		j.Target = 40 * time.Millisecond
	}
	if j.Min <= 0 {
		j.Min = 20 * time.Millisecond
	}
	if j.Max <= 0 {
		j.Max = 200 * time.Millisecond
	}
	j.Target = roundUpFrame(j.Target)
	j.Min = roundUpFrame(j.Min)
	j.Max = roundUpFrame(j.Max)
	if j.Target > j.Max {
		j.Target = j.Max
	}
	if j.Min > j.Target {
		j.Min = j.Target
	}
	return j
}

func roundUpFrame(d time.Duration) time.Duration {
	if d%tickInterval == 0 {
		return d
	}
	return (d/tickInterval + 1) * tickInterval
}
