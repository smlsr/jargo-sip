package sip

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

var (
	// ErrNATMode is returned when NATMode is not behind-sbc or standalone.
	ErrNATMode = errors.New("NATMode must be behind-sbc or standalone")
	// ErrSTUNBehindSBC is returned when behind-sbc is combined with STUN.
	ErrSTUNBehindSBC = errors.New("STUN is refused when NATMode is behind-sbc")
	// ErrPublicAdvertiseBehindSBC is returned when behind-sbc sets a public AdvertiseIP.
	ErrPublicAdvertiseBehindSBC = errors.New("public AdvertiseIP is refused when NATMode is behind-sbc")
	// ErrEmptyDomain is returned when Domain is empty.
	ErrEmptyDomain = errors.New("domain is empty")
	// ErrRTPRange is returned when the RTP range is odd, inverted, or cannot hold a pair.
	ErrRTPRange = errors.New("RTP port range is odd or inverted")
	// ErrAllowFromEmpty is returned when AllowFrom has no entry.
	ErrAllowFromEmpty = errors.New("AllowFrom is empty")
	// ErrLatchMatch is returned when Latch.Match is not port, address, or off.
	ErrLatchMatch = errors.New("Latch.Match is unknown")
	// ErrAllowRTPFrom is returned when an AllowRTPFrom entry is not an IPv4 address.
	ErrAllowRTPFrom = errors.New("AllowRTPFrom is not an IPv4 address")
	// ErrRequireAuth is returned when RequireAuth is set without a password.
	ErrRequireAuth = errors.New("RequireAuth needs a password")
)

type prepared struct {
	cfg       Config
	bindIP    net.IP
	advertise net.IP // nil when AdvertiseIP is empty
	stun      string
	udp       bool
	tcp       bool
	tls       bool
}

func prepare(cfg Config) (prepared, error) {
	cfg.ListenIP = strings.TrimSpace(cfg.ListenIP)
	cfg.AdvertiseIP = strings.TrimSpace(cfg.AdvertiseIP)
	cfg.STUNServer = strings.TrimSpace(cfg.STUNServer)
	cfg.Domain = strings.TrimSpace(cfg.Domain)
	cfg.User = strings.TrimSpace(cfg.User)
	cfg.DisplayName = strings.TrimSpace(cfg.DisplayName)
	cfg.PortMap = strings.TrimSpace(cfg.PortMap)

	cfg = applyDefaults(cfg)

	switch cfg.NATMode {
	case NATBehindSBC, NATStandalone:
	default:
		return prepared{}, ErrNATMode
	}
	if cfg.Domain == "" {
		return prepared{}, ErrEmptyDomain
	}
	allow := make([]string, 0, len(cfg.AllowFrom))
	for _, src := range cfg.AllowFrom {
		src = strings.TrimSpace(src)
		if src != "" {
			allow = append(allow, src)
		}
	}
	if len(allow) == 0 {
		return prepared{}, ErrAllowFromEmpty
	}
	cfg.AllowFrom = allow

	if err := checkRTP(cfg.RTPPortMin, cfg.RTPPortMax); err != nil {
		return prepared{}, err
	}
	if cfg.SIPPort < 1 || cfg.SIPPort > 65535 {
		return prepared{}, fmt.Errorf("SIP port %d is out of range", cfg.SIPPort)
	}

	udp, tcp, tlsOn, transports, err := normalizeTransports(cfg.Transports)
	if err != nil {
		return prepared{}, err
	}
	cfg.Transports = transports
	if err := checkTLS(&cfg, tlsOn); err != nil {
		return prepared{}, err
	}

	switch cfg.PortMap {
	case PortMapIdentity, PortMapSTUN:
	default:
		return prepared{}, fmt.Errorf("PortMap %q is unknown", cfg.PortMap)
	}

	bindIP, err := parseListen(cfg.ListenIP)
	if err != nil {
		return prepared{}, err
	}
	advertise, err := parseAdvertise(cfg.AdvertiseIP)
	if err != nil {
		return prepared{}, err
	}

	switch cfg.Latch.Match {
	case LatchMatchPort, LatchMatchAddress, LatchMatchOff:
	default:
		return prepared{}, fmt.Errorf("%w: %q", ErrLatchMatch, cfg.Latch.Match)
	}
	allowRTP, err := parseAllowRTP(cfg.Latch.AllowRTPFrom)
	if err != nil {
		return prepared{}, err
	}
	if len(allowRTP) > 0 {
		cfg.Latch.AllowRTPFrom = make([]string, len(allowRTP))
		for i, ip := range allowRTP {
			cfg.Latch.AllowRTPFrom[i] = ip.String()
		}
	}

	if cfg.NATMode == NATBehindSBC {
		if cfg.PortMap == PortMapSTUN || cfg.STUNServer != "" {
			return prepared{}, ErrSTUNBehindSBC
		}
		if advertise != nil && isPublicIP(advertise) {
			return prepared{}, ErrPublicAdvertiseBehindSBC
		}
		cfg.PortMap = PortMapIdentity
	}

	if cfg.RequireAuth && (cfg.Auth == nil || strings.TrimSpace(cfg.Auth.Password) == "") {
		return prepared{}, ErrRequireAuth
	}

	needsSTUN := cfg.NATMode == NATStandalone && (advertise == nil || cfg.PortMap == PortMapSTUN)
	if needsSTUN {
		if !udp {
			return prepared{}, errors.New("STUN requires a UDP listen socket")
		}
		if cfg.STUNServer == "" {
			return prepared{}, errors.New("STUNServer is required")
		}
	}

	return prepared{
		cfg:       cfg,
		bindIP:    cloneIP(bindIP),
		advertise: cloneIP(advertise),
		stun:      cfg.STUNServer,
		udp:       udp,
		tcp:       tcp,
		tls:       tlsOn,
	}, nil
}

func checkTLS(cfg *Config, on bool) error {
	if cfg.TLS != nil {
		cp := *cfg.TLS
		cp.CertFile = strings.TrimSpace(cp.CertFile)
		cp.KeyFile = strings.TrimSpace(cp.KeyFile)
		cfg.TLS = &cp
	}
	if !on {
		return nil
	}
	if cfg.TLS == nil {
		return errors.New("tls needs a certificate")
	}
	hasCert := cfg.TLS.CertFile != ""
	hasKey := cfg.TLS.KeyFile != ""
	if hasCert != hasKey {
		return errors.New("tls certificate and key must both be set")
	}
	if !hasCert && !cfg.TLS.SelfSigned {
		return errors.New("tls needs a certificate")
	}
	return nil
}

func parseAllowRTP(list []string) ([]net.IP, error) {
	var out []net.IP
	for _, raw := range list {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		ip := net.ParseIP(s)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("%w: %q", ErrAllowRTPFrom, raw)
		}
		out = append(out, cloneIP(ip))
	}
	return out, nil
}

func checkRTP(min, max int) error {
	if min < 1 || max > 65535 || min > max {
		return fmt.Errorf("%w (%d-%d)", ErrRTPRange, min, max)
	}
	if min%2 != 0 || max%2 != 0 {
		return fmt.Errorf("%w (%d-%d)", ErrRTPRange, min, max)
	}
	// Both ends are even and min <= max, so the span holds at least one even port.
	// RTCP is the next port and may sit one past RTPPortMax.
	return nil
}

func normalizeTransports(in []string) (udp, tcp, tlsOn bool, out []string, err error) {
	if len(in) == 0 {
		in = []string{"udp"}
	}
	seen := map[string]bool{}
	for _, raw := range in {
		name := strings.ToLower(strings.TrimSpace(raw))
		switch name {
		case "udp":
			udp = true
		case "tcp":
			tcp = true
		case "tls":
			tlsOn = true
		default:
			return false, false, false, nil, fmt.Errorf("transport %q is unknown", raw)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if !udp && !tcp && !tlsOn {
		return false, false, false, nil, errors.New("no SIP transport is configured")
	}
	return udp, tcp, tlsOn, out, nil
}
