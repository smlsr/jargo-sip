package sip

import (
	"fmt"
	"net"
)

func parseListen(s string) (net.IP, error) {
	if s == "" {
		return net.IPv4(0, 0, 0, 0), nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("ListenIP %q is not an IP address", s)
	}
	return cloneIP(ip), nil
}

func parseAdvertise(s string) (net.IP, error) {
	if s == "" {
		return nil, nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("AdvertiseIP %q is not an IP address", s)
	}
	ip = cloneIP(ip)
	if ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
		return nil, fmt.Errorf("AdvertiseIP %q is not a usable address", s)
	}
	return ip, nil
}

// isPublicIP reports whether ip is a globally routable unicast address.
// Private, loopback, link-local, unspecified, multicast, and shared
// CGNAT space (100.64.0.0/10) are not public.
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || !ip.IsGlobalUnicast() {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return true
	}
	// RFC 6598 shared address space.
	if v4[0] == 100 && v4[1]&0xc0 == 0x40 {
		return false
	}
	return true
}

func cloneIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		out := make(net.IP, net.IPv4len)
		copy(out, v4)
		return out
	}
	out := make(net.IP, len(ip))
	copy(out, ip)
	return out
}

// advertiseLocal returns the address to publish behind an SBC.
// A specific bind address is used as-is. A wildcard bind uses the
// source address the host would pick for an outbound UDP socket.
func advertiseLocal(bound net.IP) (net.IP, error) {
	if bound != nil && !bound.IsUnspecified() {
		return cloneIP(bound), nil
	}
	conn, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return nil, fmt.Errorf("local address: %w", err)
	}
	defer conn.Close()
	ua, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || ua.IP == nil || ua.IP.IsUnspecified() {
		return nil, fmt.Errorf("local address is unknown")
	}
	return cloneIP(ua.IP), nil
}
