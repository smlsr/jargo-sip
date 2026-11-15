package sip

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func baseConfig() Config {
	return Config{
		NATMode:     NATBehindSBC,
		ListenIP:    "127.0.0.1",
		Domain:      "sip.example.com",
		User:        "bot",
		DisplayName: "Bot",
		AllowFrom:   []string{"127.0.0.1"},
		RTPPortMin:  30000,
		RTPPortMax:  30200,
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p := c.LocalAddr().(*net.UDPAddr).Port
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func startEP(t *testing.T, cfg Config) *Endpoint {
	t.Helper()
	if cfg.SIPPort == 0 {
		cfg.SIPPort = freePort(t)
	}
	ep, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ep.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ep.Close() })
	return ep
}

func TestStartRefuses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   error
	}{
		{
			name:   "stun port map behind sbc",
			mutate: func(c *Config) { c.PortMap = PortMapSTUN },
			want:   ErrSTUNBehindSBC,
		},
		{
			name:   "stun server behind sbc",
			mutate: func(c *Config) { c.STUNServer = "127.0.0.1:3478" },
			want:   ErrSTUNBehindSBC,
		},
		{
			name:   "public advertise behind sbc",
			mutate: func(c *Config) { c.AdvertiseIP = "203.0.113.10" },
			want:   ErrPublicAdvertiseBehindSBC,
		},
		{
			name:   "empty domain",
			mutate: func(c *Config) { c.Domain = "" },
			want:   ErrEmptyDomain,
		},
		{
			name:   "blank domain",
			mutate: func(c *Config) { c.Domain = "  " },
			want:   ErrEmptyDomain,
		},
		{
			name:   "odd rtp min",
			mutate: func(c *Config) { c.RTPPortMin = 30001 },
			want:   ErrRTPRange,
		},
		{
			name:   "odd rtp max",
			mutate: func(c *Config) { c.RTPPortMax = 30201 },
			want:   ErrRTPRange,
		},
		{
			name: "inverted rtp",
			mutate: func(c *Config) {
				c.RTPPortMin = 30200
				c.RTPPortMax = 30000
			},
			want: ErrRTPRange,
		},
		{
			name:   "nil allow from",
			mutate: func(c *Config) { c.AllowFrom = nil },
			want:   ErrAllowFromEmpty,
		},
		{
			name:   "empty allow from",
			mutate: func(c *Config) { c.AllowFrom = []string{} },
			want:   ErrAllowFromEmpty,
		},
		{
			name:   "blank allow from",
			mutate: func(c *Config) { c.AllowFrom = []string{"  "} },
			want:   ErrAllowFromEmpty,
		},
		{
			name:   "unknown nat mode",
			mutate: func(c *Config) { c.NATMode = "lan" },
			want:   ErrNATMode,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.SIPPort = 5060
			tc.mutate(&cfg)
			ep, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			err = ep.Start(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Start err %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	cfg := Config{
		NATMode:   NATBehindSBC,
		Domain:    "sip.example.com",
		AllowFrom: []string{"10.0.0.1"},
	}
	p, err := prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.SIPPort != 5060 || p.cfg.RTPPortMin != 10000 || p.cfg.RTPPortMax != 20000 {
		t.Fatalf("ports sip %d rtp %d-%d", p.cfg.SIPPort, p.cfg.RTPPortMin, p.cfg.RTPPortMax)
	}
	if p.cfg.PortMap != PortMapIdentity || p.cfg.Latch.Match != LatchMatchPort {
		t.Fatalf("port map %s latch %s", p.cfg.PortMap, p.cfg.Latch.Match)
	}
	if p.cfg.Jitter.Target != 40*time.Millisecond || p.cfg.Jitter.Min != 20*time.Millisecond || p.cfg.Jitter.Max != 200*time.Millisecond {
		t.Fatalf("jitter %+v", p.cfg.Jitter)
	}
	if p.cfg.DTMF.PayloadType != 101 || p.cfg.DTMF.DigitDuration != 160*time.Millisecond || p.cfg.DTMF.InterDigitGap != 100*time.Millisecond {
		t.Fatalf("dtmf %+v", p.cfg.DTMF)
	}
	if len(p.cfg.Codecs) != 2 || p.cfg.Codecs[0] != CodecPCMU || p.cfg.Codecs[1] != CodecPCMA {
		t.Fatalf("codecs %v", p.cfg.Codecs)
	}
	if len(p.cfg.Transports) != 1 || p.cfg.Transports[0] != "udp" {
		t.Fatalf("transports %v", p.cfg.Transports)
	}
	if p.cfg.RTCP.Interval != 5*time.Second {
		t.Fatalf("rtcp %s", p.cfg.RTCP.Interval)
	}

	alone := cfg
	alone.NATMode = NATStandalone
	alone.AdvertiseIP = "203.0.113.10"
	p, err = prepare(alone)
	if err != nil {
		t.Fatal(err)
	}
	if p.cfg.Latch.Match != LatchMatchAddress {
		t.Fatalf("standalone latch %s", p.cfg.Latch.Match)
	}
}

func TestIsPublicIP(t *testing.T) {
	public := []string{"203.0.113.10", "8.8.8.8", "2001:db8::1"}
	local := []string{"10.1.2.3", "172.16.0.1", "192.168.1.1", "127.0.0.1", "169.254.1.1", "100.64.1.1", "0.0.0.0", "::1"}
	for _, s := range public {
		if !isPublicIP(net.ParseIP(s)) {
			t.Errorf("%s should be public", s)
		}
	}
	for _, s := range local {
		if isPublicIP(net.ParseIP(s)) {
			t.Errorf("%s should not be public", s)
		}
	}
}

func TestBehindSBCBindsLocal(t *testing.T) {
	cfg := baseConfig()
	cfg.AdvertiseIP = "10.9.8.7"
	cfg.Transports = []string{"udp", "tcp"}
	ep := startEP(t, cfg)
	a, err := ep.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	if a.NATMode != NATBehindSBC || a.PortMap != PortMapIdentity {
		t.Fatalf("mode %s map %s", a.NATMode, a.PortMap)
	}
	if !a.LocalIP.Equal(net.ParseIP("127.0.0.1")) || !a.AdvertiseIP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("local %v advertise %v", a.LocalIP, a.AdvertiseIP)
	}
	if a.AdvertiseSIPPort != a.LocalSIPPort || a.MappedSIPPort != 0 || a.MappedIP != nil {
		t.Fatalf("ports local %d advertise %d mapped %d ip %v", a.LocalSIPPort, a.AdvertiseSIPPort, a.MappedSIPPort, a.MappedIP)
	}
	if ep.cfg.Latch.Match != LatchMatchPort {
		t.Fatalf("latch %s", ep.cfg.Latch.Match)
	}

	probe := []byte("ping")
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: a.LocalSIPPort}
	c, err := net.DialUDP("udp", nil, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(probe); err != nil {
		t.Fatal(err)
	}
	_ = ep.sipUDP.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 16)
	n, _, err := ep.sipUDP.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("sip payload %q", buf[:n])
	}

	tcp, err := net.DialTimeout("tcp", ep.sipTCP.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tcp.Close()

	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pair.LocalRTCPPort != pair.LocalRTPPort+1 {
		t.Fatalf("local rtp %d rtcp %d", pair.LocalRTPPort, pair.LocalRTCPPort)
	}
	if pair.AdvertiseRTPPort != pair.LocalRTPPort || pair.AdvertiseRTCPPort != pair.LocalRTCPPort {
		t.Fatalf("advertised rtp %d rtcp %d", pair.AdvertiseRTPPort, pair.AdvertiseRTCPPort)
	}
	if pair.MappedRTPPort != 0 || pair.MappedRTCPPort != 0 {
		t.Fatalf("mapped ports set behind sbc: %d %d", pair.MappedRTPPort, pair.MappedRTCPPort)
	}
	if !pair.AdvertiseIP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("rtp advertise %v", pair.AdvertiseIP)
	}
}

func TestStandaloneAdvertisesConfiguredIP(t *testing.T) {
	cfg := baseConfig()
	cfg.NATMode = NATStandalone
	cfg.AdvertiseIP = "203.0.113.10"
	cfg.PortMap = PortMapIdentity
	ep := startEP(t, cfg)
	a, err := ep.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	if !a.AdvertiseIP.Equal(net.ParseIP("203.0.113.10")) {
		t.Fatalf("advertise %v", a.AdvertiseIP)
	}
	if !a.LocalIP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("local %v", a.LocalIP)
	}
	if a.AdvertiseSIPPort != a.LocalSIPPort || a.MappedSIPPort != 0 || a.PortMap != PortMapIdentity {
		t.Fatalf("addr %+v", a)
	}
	if ep.cfg.Latch.Match != LatchMatchAddress {
		t.Fatalf("latch %s", ep.cfg.Latch.Match)
	}
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pair.AdvertiseRTPPort != pair.LocalRTPPort || pair.AdvertiseRTCPPort != pair.LocalRTCPPort {
		t.Fatalf("identity mapped ports changed: local %d/%d advertise %d/%d", pair.LocalRTPPort, pair.LocalRTCPPort, pair.AdvertiseRTPPort, pair.AdvertiseRTCPPort)
	}
	if !pair.AdvertiseIP.Equal(net.ParseIP("203.0.113.10")) {
		t.Fatalf("rtp advertise %v", pair.AdvertiseIP)
	}
}

func TestStandaloneSTUNDiscoversAdvertiseIP(t *testing.T) {
	box := newSTUNBox(t)

	t.Run("stun port", func(t *testing.T) {
		cfg := baseConfig()
		cfg.NATMode = NATStandalone
		cfg.PortMap = PortMapSTUN
		cfg.STUNServer = box.addr()
		ep := startEP(t, cfg)
		a := mustAddrs(t, ep)
		if !a.AdvertiseIP.Equal(net.ParseIP("203.0.113.50")) || !a.MappedIP.Equal(a.AdvertiseIP) {
			t.Fatalf("advertise %v mapped %v", a.AdvertiseIP, a.MappedIP)
		}
		if a.AdvertiseSIPPort != mappedPort(a.LocalSIPPort) {
			t.Fatalf("sip local %d advertise %d", a.LocalSIPPort, a.AdvertiseSIPPort)
		}
	})

	t.Run("identity keeps local port", func(t *testing.T) {
		cfg := baseConfig()
		cfg.NATMode = NATStandalone
		cfg.PortMap = PortMapIdentity
		cfg.STUNServer = box.addr()
		ep := startEP(t, cfg)
		a := mustAddrs(t, ep)
		before := len(box.snapshot())
		if !a.AdvertiseIP.Equal(net.ParseIP("203.0.113.50")) {
			t.Fatalf("advertise %v", a.AdvertiseIP)
		}
		if a.AdvertiseSIPPort != a.LocalSIPPort || a.MappedSIPPort != mappedPort(a.LocalSIPPort) {
			t.Fatalf("local %d advertise %d mapped %d", a.LocalSIPPort, a.AdvertiseSIPPort, a.MappedSIPPort)
		}
		pair, err := ep.AllocateRTP(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if pair.AdvertiseRTPPort != pair.LocalRTPPort || pair.AdvertiseRTCPPort != pair.LocalRTCPPort {
			t.Fatalf("identity rtp ports local %d/%d advertise %d/%d", pair.LocalRTPPort, pair.LocalRTCPPort, pair.AdvertiseRTPPort, pair.AdvertiseRTCPPort)
		}
		hits := box.snapshot()
		if len(hits) != before {
			t.Fatalf("identity allocation sent %d extra binding requests", len(hits)-before)
		}
	})
}

func mustAddrs(t *testing.T, ep *Endpoint) Addresses {
	t.Helper()
	a, err := ep.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestStandaloneSTUNFailureReleasesSIP(t *testing.T) {
	blackhole, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	stunAddr := blackhole.LocalAddr().String()
	blackhole.Close()

	cfg := baseConfig()
	cfg.NATMode = NATStandalone
	cfg.AdvertiseIP = "203.0.113.10"
	cfg.PortMap = PortMapSTUN
	cfg.STUNServer = stunAddr
	cfg.SIPPort = freePort(t)
	ep, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := ep.Start(ctx); err == nil {
		t.Fatal("Start succeeded without a STUN server")
	}
	again, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: cfg.SIPPort})
	if err != nil {
		t.Fatalf("sip port %d still held: %v", cfg.SIPPort, err)
	}
	again.Close()
}

func TestCancelDuringSTUNReleasesPort(t *testing.T) {
	peer := dropSTUN(t)
	cfg := baseConfig()
	cfg.NATMode = NATStandalone
	cfg.AdvertiseIP = "203.0.113.10"
	cfg.PortMap = PortMapSTUN
	cfg.STUNServer = peer.LocalAddr().String()
	cfg.SIPPort = freePort(t)
	ep, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		errc <- ep.Start(parent)
	}()
	time.Sleep(150 * time.Millisecond)
	cancelled := time.Now()
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start err %v, want context canceled", err)
		}
		if waited := time.Since(cancelled); waited > time.Second {
			t.Fatalf("Start returned %s after cancel", waited)
		}
	case <-time.After(time.Second):
		t.Fatal("Start still blocked 1s after cancel")
	}
	again, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: cfg.SIPPort})
	if err != nil {
		t.Fatalf("sip port %d still held: %v", cfg.SIPPort, err)
	}
	again.Close()
}

func TestCloseDuringSTUNReleasesPort(t *testing.T) {
	peer := dropSTUN(t)
	cfg := baseConfig()
	cfg.NATMode = NATStandalone
	cfg.AdvertiseIP = "203.0.113.10"
	cfg.PortMap = PortMapSTUN
	cfg.STUNServer = peer.LocalAddr().String()
	cfg.SIPPort = freePort(t)
	ep, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- ep.Start(parent)
	}()
	time.Sleep(150 * time.Millisecond)
	closed := time.Now()
	if err := ep.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Start succeeded after Close")
		}
		if waited := time.Since(closed); waited > time.Second {
			t.Fatalf("Start returned %s after Close", waited)
		}
	case <-time.After(time.Second):
		t.Fatal("Start still blocked 1s after Close")
	}
	again, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: cfg.SIPPort})
	if err != nil {
		t.Fatalf("sip port %d still held: %v", cfg.SIPPort, err)
	}
	again.Close()
}

func TestCancelDuringAllocateRTPReleasesPort(t *testing.T) {
	box := newSTUNBox(t)
	cfg := baseConfig()
	cfg.NATMode = NATStandalone
	cfg.AdvertiseIP = "203.0.113.10"
	cfg.PortMap = PortMapSTUN
	cfg.STUNServer = box.addr()
	ep := startEP(t, cfg)
	box.silence()

	parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type allocResult struct {
		pair *RTPPair
		err  error
	}
	out := make(chan allocResult, 1)
	go func() {
		pair, err := ep.AllocateRTP(parent)
		out <- allocResult{pair, err}
	}()
	time.Sleep(150 * time.Millisecond)
	cancelled := time.Now()
	cancel()
	select {
	case got := <-out:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("AllocateRTP err %v, want context canceled", got.err)
		}
		if got.pair != nil {
			t.Fatal("AllocateRTP returned a pair")
		}
		if waited := time.Since(cancelled); waited > time.Second {
			t.Fatalf("AllocateRTP returned %s after cancel", waited)
		}
	case <-time.After(time.Second):
		t.Fatal("AllocateRTP still blocked 1s after cancel")
	}
	ep.mu.Lock()
	held := len(ep.used)
	ep.mu.Unlock()
	if held != 0 {
		t.Fatalf("%d RTP ports still reserved", held)
	}
}

func TestStandaloneSTUNBindingPerRTPSocket(t *testing.T) {
	box := newSTUNBox(t)
	cfg := baseConfig()
	cfg.NATMode = NATStandalone
	cfg.AdvertiseIP = "203.0.113.10"
	cfg.PortMap = PortMapSTUN
	cfg.STUNServer = box.addr()
	ep := startEP(t, cfg)

	a, err := ep.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	if !a.AdvertiseIP.Equal(net.ParseIP("203.0.113.10")) {
		t.Fatalf("sip advertise ip %v", a.AdvertiseIP)
	}
	if a.AdvertiseSIPPort != mappedPort(a.LocalSIPPort) || a.MappedSIPPort != a.AdvertiseSIPPort {
		t.Fatalf("sip local %d advertise %d mapped %d", a.LocalSIPPort, a.AdvertiseSIPPort, a.MappedSIPPort)
	}
	if !a.MappedIP.Equal(net.ParseIP("203.0.113.50")) {
		t.Fatalf("sip mapped ip %v", a.MappedIP)
	}

	first, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertPairSTUN(t, first)
	assertPairSTUN(t, second)
	if first.LocalRTPPort == second.LocalRTPPort {
		t.Fatalf("pairs reused port %d", first.LocalRTPPort)
	}

	ports := []int{
		a.LocalSIPPort,
		first.LocalRTPPort, first.LocalRTCPPort,
		second.LocalRTPPort, second.LocalRTCPPort,
	}
	hits := box.snapshot()
	byPort := map[int][]stunHit{}
	seenTx := map[[12]byte]int{}
	for _, h := range hits {
		byPort[h.Port] = append(byPort[h.Port], h)
		seenTx[h.Tx]++
	}
	if len(seenTx) != len(hits) {
		t.Fatalf("transaction ids reused: %d hits %d ids", len(hits), len(seenTx))
	}
	for _, port := range ports {
		got := byPort[port]
		if len(got) != 1 {
			t.Fatalf("port %d has %d binding requests, want 1 (hits %v)", port, len(got), portsOf(hits))
		}
	}
	if len(hits) != len(ports) {
		t.Fatalf("binding requests %v, want one each from %v", portsOf(hits), ports)
	}
}

func assertPairSTUN(t *testing.T, p *RTPPair) {
	t.Helper()
	if p.LocalRTCPPort != p.LocalRTPPort+1 {
		t.Fatalf("local ports %d %d", p.LocalRTPPort, p.LocalRTCPPort)
	}
	if p.AdvertiseRTPPort != mappedPort(p.LocalRTPPort) || p.MappedRTPPort != p.AdvertiseRTPPort {
		t.Fatalf("rtp local %d advertise %d mapped %d", p.LocalRTPPort, p.AdvertiseRTPPort, p.MappedRTPPort)
	}
	if p.AdvertiseRTCPPort != mappedPort(p.LocalRTCPPort) || p.MappedRTCPPort != p.AdvertiseRTCPPort {
		t.Fatalf("rtcp local %d advertise %d mapped %d", p.LocalRTCPPort, p.AdvertiseRTCPPort, p.MappedRTCPPort)
	}
	if p.AdvertiseRTPPort == p.AdvertiseRTCPPort {
		t.Fatalf("rtp and rtcp advertised the same port %d", p.AdvertiseRTPPort)
	}
	if p.AdvertiseRTPPort+1 == p.AdvertiseRTCPPort {
		t.Fatalf("mapped ports stayed consecutive %d %d", p.AdvertiseRTPPort, p.AdvertiseRTCPPort)
	}
	if !p.AdvertiseIP.Equal(net.ParseIP("203.0.113.10")) {
		t.Fatalf("advertise ip %v", p.AdvertiseIP)
	}
	if !p.MappedRTPIP.Equal(net.ParseIP("203.0.113.50")) || !p.MappedRTCIP.Equal(net.ParseIP("203.0.113.50")) {
		t.Fatalf("mapped ips %v %v", p.MappedRTPIP, p.MappedRTCIP)
	}
}

// mappedPort is the test server's per-socket mapping. Even and odd
// sources are shifted by different amounts so the two results cannot
// be one reused response.
func mappedPort(src int) int {
	if src%2 == 0 {
		return src + 1000
	}
	return src + 2222
}

type stunHit struct {
	Port int
	Tx   [12]byte
}

func dropSTUN(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := conn.ReadFromUDP(buf); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

type stunBox struct {
	conn   *net.UDPConn
	mu     sync.Mutex
	hits   []stunHit
	silent bool
}

func newSTUNBox(t *testing.T) *stunBox {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	b := &stunBox{conn: conn}
	go b.serve()
	t.Cleanup(func() { _ = conn.Close() })
	return b
}

func (b *stunBox) addr() string {
	return b.conn.LocalAddr().String()
}

func (b *stunBox) silence() {
	b.mu.Lock()
	b.silent = true
	b.mu.Unlock()
}

func (b *stunBox) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < stunHeaderLen || buf[0]&0xC0 != 0 || binary.BigEndian.Uint32(buf[4:8]) != stunMagic {
			continue
		}
		if binary.BigEndian.Uint16(buf[0:2]) != stunBindingReq {
			continue
		}
		var tx [12]byte
		copy(tx[:], buf[8:20])
		ip := net.ParseIP("203.0.113.50")
		port := mappedPort(from.Port)
		msg, err := encodeSuccess(tx, ip, port)
		if err != nil {
			continue
		}
		b.mu.Lock()
		b.hits = append(b.hits, stunHit{Port: from.Port, Tx: tx})
		silent := b.silent
		b.mu.Unlock()
		if silent {
			continue
		}
		_, _ = b.conn.WriteToUDP(msg, from)
	}
}

func (b *stunBox) snapshot() []stunHit {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]stunHit, len(b.hits))
	copy(out, b.hits)
	return out
}

func portsOf(hits []stunHit) []int {
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = h.Port
	}
	return out
}
