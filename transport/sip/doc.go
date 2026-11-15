// Package sip binds one SIP endpoint and resolves the addresses it advertises.
//
// NATMode behind-sbc advertises the local IP and port. STUN is refused, and a
// public AdvertiseIP is refused. NATMode standalone advertises Config.AdvertiseIP
// when it is set, otherwise the mapped IP from a STUN Binding (RFC 8489) sent
// on the SIP UDP socket. PortMap identity advertises the local port. PortMap
// stun advertises that socket's mapped port. Each RTP socket sends its own
// Binding request; a mapped address is not reused across sockets.
//
// Dialogs, SDP, and media are not part of this package yet.
package sip
