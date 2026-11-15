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
// There is no a=rtcp-mux.
//
// StartMedia runs one RTP session on an allocated pair. The sender uses
// github.com/pion/rtp v1.10.6 and a 20 ms clock at 8000 Hz. RTCP uses
// github.com/pion/rtcp v1.2.20 on the paired socket and its own latch.
// PCMU and PCMA bytes come from github.com/gojargo/jargo/audio/g711 v0.6.1
// (2026-11-12, 7d0492d2c0febfaa0c140007317992f84c8c1be9). Codec silence on
// the wire is µ-law 0xFF or A-law 0xD5.
//
// ReadPCM plays one 20 ms frame from a sequence ring. The head waits until
// Target of audio is buffered, or the oldest packet is older than Target.
// A missing packet repeats the previous frame. Pipeline rates other than
// 8000 build one audio/resample stream per direction; 8000 does not.
//
// Telephone-event follows RFC 4733. ReadDTMF emits one event per press.
// WriteDTMF replaces audio for that press, then sends three end packets.
// A 183 with SDP starts early media into ReadPCM. A 200 that repeats the
// address keeps that sender and the frames already queued. The basic UAS
// path does not generate a 183.
//
// Dialogs are not part of this package yet.
package sip
