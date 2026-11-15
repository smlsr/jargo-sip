package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

// Addresses is the bound SIP socket and the address the endpoint advertises.
// MappedIP and MappedSIPPort come from STUN on the SIP socket. They are nil
// and zero when STUN did not run.
type Addresses struct {
	NATMode          NATMode
	PortMap          string
	ListenIP         net.IP
	LocalIP          net.IP
	LocalSIPPort     int
	AdvertiseIP      net.IP
	AdvertiseSIPPort int
	MappedIP         net.IP
	MappedSIPPort    int
}

// Endpoint is one SIP port. Start binds it. AllocateRTP binds an RTP pair.
type Endpoint struct {
	mu       sync.Mutex
	cfg      Config
	starting bool
	started  bool
	closed   bool
	sipUDP   *net.UDPConn
	sipTCP   *net.TCPListener
	addrs    Addresses
	stun     *net.UDPAddr
	bindIP   net.IP
	rtpMin   int
	rtpMax   int
	used     map[int]struct{}
	pairs    []*RTPPair
}

// New stores cfg. Start validates it and binds.
func New(cfg Config) (*Endpoint, error) {
	return &Endpoint{cfg: cfg}, nil
}

// Start checks cfg, binds the SIP transports, and resolves the advertised address.
func (e *Endpoint) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("endpoint is closed")
	}
	if e.started || e.starting {
		e.mu.Unlock()
		return errors.New("endpoint is already started")
	}
	cfg := e.cfg
	e.starting = true
	e.mu.Unlock()

	var udp *net.UDPConn
	var tcp *net.TCPListener
	owned := false
	defer func() {
		if owned {
			return
		}
		closeBind(udp, tcp)
		e.mu.Lock()
		e.starting = false
		e.mu.Unlock()
	}()

	ready, err := prepare(cfg)
	if err != nil {
		return err
	}
	udp, tcp, port, err := listenSIP(ready)
	if err != nil {
		return err
	}
	localIP, err := advertiseLocal(socketIP(udp, tcp))
	if err != nil {
		return err
	}

	advIP := localIP
	advPort := port
	var mappedIP net.IP
	var mappedPort int
	var stunAddr *net.UDPAddr
	if ready.cfg.NATMode == NATStandalone {
		if ready.advertise != nil {
			advIP = cloneIP(ready.advertise)
		}
		if ready.advertise == nil || ready.cfg.PortMap == PortMapSTUN {
			if udp == nil {
				return errors.New("STUN requires a UDP listen socket")
			}
			stunAddr, err = resolveSTUN(ready.stun)
			if err != nil {
				return err
			}
			mip, mp, berr := binding(ctx, udp, stunAddr)
			if berr != nil {
				return berr
			}
			mappedIP = mip
			mappedPort = mp
			if ready.advertise == nil {
				advIP = cloneIP(mip)
			}
			if ready.cfg.PortMap == PortMapSTUN {
				advPort = mp
			}
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("endpoint is closed")
	}
	e.cfg = ready.cfg
	e.bindIP = cloneIP(ready.bindIP)
	e.stun = stunAddr
	e.sipUDP = udp
	e.sipTCP = tcp
	e.rtpMin = ready.cfg.RTPPortMin
	e.rtpMax = ready.cfg.RTPPortMax
	e.used = map[int]struct{}{}
	e.addrs = Addresses{
		NATMode:          ready.cfg.NATMode,
		PortMap:          ready.cfg.PortMap,
		ListenIP:         cloneIP(ready.bindIP),
		LocalIP:          cloneIP(localIP),
		LocalSIPPort:     port,
		AdvertiseIP:      cloneIP(advIP),
		AdvertiseSIPPort: advPort,
		MappedIP:         cloneIP(mappedIP),
		MappedSIPPort:    mappedPort,
	}
	e.started = true
	e.starting = false
	owned = true
	return nil
}

// Addresses returns a copy of the bound and advertised SIP addresses.
func (e *Endpoint) Addresses() (Addresses, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return Addresses{}, errors.New("endpoint is not started")
	}
	a := e.addrs
	a.ListenIP = cloneIP(a.ListenIP)
	a.LocalIP = cloneIP(a.LocalIP)
	a.AdvertiseIP = cloneIP(a.AdvertiseIP)
	a.MappedIP = cloneIP(a.MappedIP)
	return a, nil
}

// Close releases the SIP sockets and any RTP pairs still held.
func (e *Endpoint) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	e.started = false
	err := closeBind(e.sipUDP, e.sipTCP)
	e.sipUDP = nil
	e.sipTCP = nil
	pairs := append([]*RTPPair(nil), e.pairs...)
	for _, p := range pairs {
		if cerr := p.closeLocked(); err == nil {
			err = cerr
		}
	}
	e.pairs = nil
	return err
}

func listenSIP(p prepared) (*net.UDPConn, *net.TCPListener, int, error) {
	port := p.cfg.SIPPort
	var udp *net.UDPConn
	var tcp *net.TCPListener
	if p.udp {
		addr := &net.UDPAddr{IP: p.bindIP, Port: port}
		var err error
		udp, err = net.ListenUDP("udp", addr)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("listen udp %s: %w", addr, err)
		}
		got, ok := udp.LocalAddr().(*net.UDPAddr)
		if !ok {
			udp.Close()
			return nil, nil, 0, errors.New("udp listen address is missing")
		}
		port = got.Port
	}
	if p.tcp {
		addr := &net.TCPAddr{IP: p.bindIP, Port: port}
		var err error
		tcp, err = net.ListenTCP("tcp", addr)
		if err != nil {
			closeBind(udp, nil)
			return nil, nil, 0, fmt.Errorf("listen tcp %s: %w", addr, err)
		}
		if !p.udp {
			got, ok := tcp.Addr().(*net.TCPAddr)
			if !ok {
				tcp.Close()
				return nil, nil, 0, errors.New("tcp listen address is missing")
			}
			port = got.Port
		}
	}
	return udp, tcp, port, nil
}

func socketIP(udp *net.UDPConn, tcp *net.TCPListener) net.IP {
	if udp != nil {
		if a, ok := udp.LocalAddr().(*net.UDPAddr); ok {
			return a.IP
		}
	}
	if tcp != nil {
		if a, ok := tcp.Addr().(*net.TCPAddr); ok {
			return a.IP
		}
	}
	return nil
}

func closeBind(udp *net.UDPConn, tcp *net.TCPListener) error {
	var err error
	if udp != nil {
		err = udp.Close()
	}
	if tcp != nil {
		if cerr := tcp.Close(); err == nil {
			err = cerr
		}
	}
	return err
}
