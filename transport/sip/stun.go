package sip

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// RFC 8489 / RFC 5389 header and the XOR-MAPPED-ADDRESS attribute.
const (
	stunHeaderLen  = 20
	stunMagic      = 0x2112A442
	stunMagicPort  = 0x2112
	stunBindingReq = 0x0001
	stunBindingOK  = 0x0101
	stunBindingErr = 0x0111
	attrXORMapped  = 0x0020
	stunFamilyV4   = 0x01
	stunFamilyV6   = 0x02
)

var errNotSTUN = errors.New("not a STUN message")

func resolveSTUN(server string) (*net.UDPAddr, error) {
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(server, "3478")
	}
	addr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return nil, fmt.Errorf("STUN server: %w", err)
	}
	return addr, nil
}

// binding sends one Binding request on conn and returns the mapped address
// from that response. The result belongs to conn alone.
//
// The read deadline is the sooner of 2s and ctx's deadline. Cancel wakes the
// read immediately with SetReadDeadline(time.Now()); closing conn does too.
func binding(ctx context.Context, conn *net.UDPConn, server *net.UDPAddr) (net.IP, int, error) {
	if ctx == nil {
		return nil, 0, errors.New("nil context")
	}
	ctx, cancel := bindCtx(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	raw, tx, err := newBindingRequest()
	if err != nil {
		return nil, 0, err
	}
	if _, err := conn.WriteToUDP(raw, server); err != nil {
		return nil, 0, fmt.Errorf("stun binding: %w", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(2 * time.Second)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, 0, err
	}
	stopWake := wakeOnCancel(ctx, conn)
	defer func() {
		stopWake()
		_ = conn.SetReadDeadline(time.Time{})
	}()

	buf := make([]byte, 1500)
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			return nil, 0, fmt.Errorf("stun binding: %w", err)
		}
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		if from == nil || !from.IP.Equal(server.IP) || from.Port != server.Port {
			continue
		}
		got, ip, port, kind, err := inspectSTUN(buf[:n])
		if errors.Is(err, errNotSTUN) || kind == stunSkip {
			continue
		}
		if got != tx {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if kind != stunOK {
			return nil, 0, fmt.Errorf("stun binding rejected")
		}
		return ip, port, nil
	}
}

// bindCtx limits one Binding exchange to 2s. WithTimeout's deadline is the
// sooner of that cap and ctx's own deadline, and the returned cancel is live
// even when ctx already has a deadline.
func bindCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 2*time.Second)
}

// wakeOnCancel unblocks a ReadFromUDP when ctx is done. stop waits until the
// watcher has finished touching the socket.
func wakeOnCancel(ctx context.Context, conn *net.UDPConn) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-done:
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

func newBindingRequest() ([]byte, [12]byte, error) {
	raw := make([]byte, stunHeaderLen)
	binary.BigEndian.PutUint16(raw[0:2], stunBindingReq)
	binary.BigEndian.PutUint32(raw[4:8], stunMagic)
	var tx [12]byte
	if _, err := rand.Read(raw[8:20]); err != nil {
		return nil, tx, err
	}
	copy(tx[:], raw[8:20])
	return raw, tx, nil
}

const (
	stunSkip = iota
	stunOK
	stunFail
)

func inspectSTUN(buf []byte) (tx [12]byte, ip net.IP, port int, kind int, err error) {
	if len(buf) < stunHeaderLen || buf[0]&0xC0 != 0 {
		return tx, nil, 0, stunSkip, errNotSTUN
	}
	mlen := int(binary.BigEndian.Uint16(buf[2:4]))
	if mlen%4 != 0 || stunHeaderLen+mlen > len(buf) {
		return tx, nil, 0, stunSkip, errNotSTUN
	}
	if binary.BigEndian.Uint32(buf[4:8]) != stunMagic {
		return tx, nil, 0, stunSkip, errNotSTUN
	}
	copy(tx[:], buf[8:20])
	typ := binary.BigEndian.Uint16(buf[0:2])
	switch typ {
	case stunBindingOK:
		ip, port, err = xorMapped(buf[stunHeaderLen:stunHeaderLen+mlen], tx)
		if err != nil {
			return tx, nil, 0, stunFail, err
		}
		return tx, ip, port, stunOK, nil
	case stunBindingErr:
		return tx, nil, 0, stunFail, errors.New("stun binding error response")
	default:
		return tx, nil, 0, stunSkip, nil
	}
}

func xorMapped(attrs []byte, tx [12]byte) (net.IP, int, error) {
	off := 0
	for off+4 <= len(attrs) {
		typ := binary.BigEndian.Uint16(attrs[off : off+2])
		n := int(binary.BigEndian.Uint16(attrs[off+2 : off+4]))
		val := off + 4
		next := val + ((n + 3) &^ 3)
		if next > len(attrs) {
			return nil, 0, errors.New("stun attribute overruns message")
		}
		if typ == attrXORMapped {
			return unxorAddr(attrs[val:val+n], tx)
		}
		off = next
	}
	return nil, 0, errors.New("stun response has no XOR-MAPPED-ADDRESS")
}

func unxorAddr(value []byte, tx [12]byte) (net.IP, int, error) {
	if len(value) < 4 {
		return nil, 0, errors.New("stun XOR-MAPPED-ADDRESS is short")
	}
	port := int(binary.BigEndian.Uint16(value[2:4]) ^ stunMagicPort)
	if port == 0 {
		return nil, 0, errors.New("stun mapped port is 0")
	}
	switch value[1] {
	case stunFamilyV4:
		if len(value) < 8 {
			return nil, 0, errors.New("stun XOR-MAPPED-ADDRESS is short")
		}
		raw := binary.BigEndian.Uint32(value[4:8]) ^ stunMagic
		ip := make(net.IP, net.IPv4len)
		binary.BigEndian.PutUint32(ip, raw)
		return ip, port, nil
	case stunFamilyV6:
		if len(value) < 20 {
			return nil, 0, errors.New("stun XOR-MAPPED-ADDRESS is short")
		}
		ip := make(net.IP, net.IPv6len)
		copy(ip, value[4:20])
		var key [16]byte
		binary.BigEndian.PutUint32(key[0:4], stunMagic)
		copy(key[4:], tx[:])
		for i := range ip {
			ip[i] ^= key[i]
		}
		return ip, port, nil
	default:
		return nil, 0, fmt.Errorf("stun address family %d", value[1])
	}
}

func encodeSuccess(tx [12]byte, ip net.IP, port int) ([]byte, error) {
	val, err := xorValue(tx, ip, port)
	if err != nil {
		return nil, err
	}
	body := putAttr(nil, attrXORMapped, val)
	msg := make([]byte, stunHeaderLen+len(body))
	binary.BigEndian.PutUint16(msg[0:2], stunBindingOK)
	binary.BigEndian.PutUint16(msg[2:4], uint16(len(body)))
	binary.BigEndian.PutUint32(msg[4:8], stunMagic)
	copy(msg[8:20], tx[:])
	copy(msg[20:], body)
	return msg, nil
}

func xorValue(tx [12]byte, ip net.IP, port int) ([]byte, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("stun port %d", port)
	}
	xport := uint16(port) ^ stunMagicPort
	if v4 := ip.To4(); v4 != nil {
		val := make([]byte, 8)
		val[1] = stunFamilyV4
		binary.BigEndian.PutUint16(val[2:4], xport)
		raw := binary.BigEndian.Uint32(v4) ^ stunMagic
		binary.BigEndian.PutUint32(val[4:8], raw)
		return val, nil
	}
	if len(ip) != net.IPv6len {
		return nil, errors.New("stun address is empty")
	}
	val := make([]byte, 20)
	val[1] = stunFamilyV6
	binary.BigEndian.PutUint16(val[2:4], xport)
	copy(val[4:], ip)
	var key [16]byte
	binary.BigEndian.PutUint32(key[0:4], stunMagic)
	copy(key[4:], tx[:])
	for i := 0; i < net.IPv6len; i++ {
		val[4+i] ^= key[i]
	}
	return val, nil
}

func putAttr(dst []byte, typ uint16, val []byte) []byte {
	hdr := len(dst)
	dst = append(dst, 0, 0, 0, 0)
	binary.BigEndian.PutUint16(dst[hdr:], typ)
	binary.BigEndian.PutUint16(dst[hdr+2:], uint16(len(val)))
	dst = append(dst, val...)
	if pad := (4 - len(val)%4) % 4; pad != 0 {
		dst = append(dst, make([]byte, pad)...)
	}
	return dst
}
