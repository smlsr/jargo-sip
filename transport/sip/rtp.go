package sip

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// RTPPair is one RTP socket and its RTCP socket (local port + 1).
// Advertise ports are the local ports, or each socket's STUN mapped port
// when PortMap is stun.
type RTPPair struct {
	endpoint *Endpoint

	RTP  *net.UDPConn
	RTCP *net.UDPConn

	LocalIP       net.IP
	LocalRTPPort  int
	LocalRTCPPort int

	AdvertiseIP       net.IP
	AdvertiseRTPPort  int
	AdvertiseRTCPPort int

	MappedRTPIP    net.IP
	MappedRTCIP    net.IP
	MappedRTPPort  int
	MappedRTCPPort int

	released bool
}

// AllocateRTP binds the next even RTP port and the following RTCP port.
// In standalone mode with PortMap stun, the RTP socket and the RTCP socket
// each send their own Binding request.
func (e *Endpoint) AllocateRTP(ctx context.Context) (*RTPPair, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	e.mu.Lock()
	if e.closed || !e.started {
		e.mu.Unlock()
		return nil, errors.New("endpoint is not started")
	}
	bindIP := cloneIP(e.bindIP)
	advIP := cloneIP(e.addrs.AdvertiseIP)
	portMap := e.addrs.PortMap
	server := e.stun
	e.mu.Unlock()

	rtpConn, rtcpConn, rtpPort, err := e.bindFreePair(bindIP)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*RTPPair, error) {
		e.abortSockets(rtpConn, rtcpConn, rtpPort)
		return nil, cause
	}

	localRTP, err := udpPort(rtpConn)
	if err != nil {
		return fail(err)
	}
	localRTCP, err := udpPort(rtcpConn)
	if err != nil {
		return fail(err)
	}
	if err := e.holdConns(rtpConn, rtcpConn); err != nil {
		return fail(err)
	}
	defer e.releaseConns(rtpConn, rtcpConn)

	pair := &RTPPair{
		RTP:           rtpConn,
		RTCP:          rtcpConn,
		LocalIP:       cloneIP(socketIP(rtpConn, nil)),
		LocalRTPPort:  localRTP,
		LocalRTCPPort: localRTCP,
		AdvertiseIP:   advIP,
	}
	if portMap == PortMapSTUN {
		if server == nil {
			return fail(errors.New("STUN server is not set"))
		}
		rip, rp, berr := binding(ctx, rtpConn, server)
		if berr != nil {
			return fail(fmt.Errorf("rtp stun: %w", berr))
		}
		cip, cp, berr := binding(ctx, rtcpConn, server)
		if berr != nil {
			return fail(fmt.Errorf("rtcp stun: %w", berr))
		}
		pair.AdvertiseRTPPort = rp
		pair.AdvertiseRTCPPort = cp
		pair.MappedRTPIP = rip
		pair.MappedRTCIP = cip
		pair.MappedRTPPort = rp
		pair.MappedRTCPPort = cp
	} else {
		pair.AdvertiseRTPPort = localRTP
		pair.AdvertiseRTCPPort = localRTCP
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || !e.started {
		_ = rtpConn.Close()
		_ = rtcpConn.Close()
		delete(e.used, rtpPort)
		delete(e.used, rtpPort+1)
		return nil, errors.New("endpoint is not started")
	}
	pair.endpoint = e
	e.pairs = append(e.pairs, pair)
	return pair, nil
}

// Close releases the pair's sockets and its ports.
func (p *RTPPair) Close() error {
	if p == nil || p.endpoint == nil {
		return nil
	}
	p.endpoint.mu.Lock()
	defer p.endpoint.mu.Unlock()
	return p.closeLocked()
}

func (e *Endpoint) bindFreePair(ip net.IP) (*net.UDPConn, *net.UDPConn, int, error) {
	skip := map[int]bool{}
	var last error
	for {
		port, err := e.reserve(skip)
		if err != nil {
			if last != nil {
				return nil, nil, 0, last
			}
			return nil, nil, 0, err
		}
		rtp, rtcp, berr := bindPair(ip, port)
		if berr != nil {
			e.release(port)
			skip[port] = true
			last = berr
			continue
		}
		return rtp, rtcp, port, nil
	}
}

func bindPair(ip net.IP, port int) (*net.UDPConn, *net.UDPConn, error) {
	rtp, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return nil, nil, err
	}
	got, gerr := udpPort(rtp)
	if gerr != nil || got != port {
		rtp.Close()
		if gerr != nil {
			return nil, nil, gerr
		}
		return nil, nil, fmt.Errorf("rtp bound %d, wanted %d", got, port)
	}
	rtcp, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: port + 1})
	if err != nil {
		rtp.Close()
		return nil, nil, err
	}
	return rtp, rtcp, nil
}

func (e *Endpoint) reserve(skip map[int]bool) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || !e.started {
		return 0, errors.New("endpoint is not started")
	}
	for p := e.rtpMin; p+1 <= e.rtpMax; p += 2 {
		if skip[p] {
			continue
		}
		if _, ok := e.used[p]; ok {
			continue
		}
		if _, ok := e.used[p+1]; ok {
			continue
		}
		e.used[p] = struct{}{}
		e.used[p+1] = struct{}{}
		return p, nil
	}
	return 0, errors.New("RTP port range is exhausted")
}

func (e *Endpoint) release(port int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.used, port)
	delete(e.used, port+1)
}

func (e *Endpoint) abortSockets(rtp, rtcp *net.UDPConn, port int) {
	if rtp != nil {
		_ = rtp.Close()
	}
	if rtcp != nil {
		_ = rtcp.Close()
	}
	e.release(port)
}

func (p *RTPPair) closeLocked() error {
	if p.released {
		return nil
	}
	p.released = true
	if p.endpoint != nil {
		delete(p.endpoint.used, p.LocalRTPPort)
		delete(p.endpoint.used, p.LocalRTCPPort)
		p.endpoint.forget(p)
	}
	var err error
	if p.RTP != nil {
		err = p.RTP.Close()
	}
	if p.RTCP != nil {
		if cerr := p.RTCP.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

func (e *Endpoint) forget(p *RTPPair) {
	n := 0
	for _, q := range e.pairs {
		if q != p {
			e.pairs[n] = q
			n++
		}
	}
	for i := n; i < len(e.pairs); i++ {
		e.pairs[i] = nil
	}
	e.pairs = e.pairs[:n]
}

func udpPort(c *net.UDPConn) (int, error) {
	a, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || a == nil {
		return 0, errors.New("udp address is missing")
	}
	return a.Port, nil
}
