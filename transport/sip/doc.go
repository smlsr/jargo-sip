// Package sip binds one SIP endpoint, resolves the addresses it advertises,
// reads and writes SDP, and allocates even RTP ports.
//
// NATMode behind-sbc advertises the local IP and port. STUN is refused, and a
// public AdvertiseIP is refused. NATMode standalone advertises Config.AdvertiseIP
// when it is set, otherwise the mapped IP from a STUN Binding (RFC 8489) sent
// on the SIP UDP socket. PortMap identity advertises the local port. PortMap
// stun advertises that socket's mapped port. Each RTP socket sends its own
// Binding request; a mapped address is not reused across sockets.
//
// An offer lists Config.Codecs (PCMU then PCMA by default), then
// telephone-event/8000. a=rtcp is written only when the RTP and RTCP ports
// are not consecutive. The allocator returns the lowest free even port in
// [RTPPortMin, RTPPortMax] and reserves the next port for RTCP.
//
// Dialogs and the media sender are not part of this package yet.
package sip
