# NAT and STUN

Owner: JAR-3. The bind socket is always local. Only the advertised address changes.

## behind-sbc

The SBC is on the same L3 network. Via, Contact, SDP `c=`, and SDP `m=` use the local IP and the local port. There is no STUN. A configured `AdvertiseIP` is a configuration error and `Start` refuses it. `STUNServer` non-empty and `PortMap: stun` are the same refusal. `PortMap` is forced to `identity`.

The local IP is `ListenIP` when that field is a specific address. When `ListenIP` is empty the socket binds `0.0.0.0`, and the IP written into Via, Contact, and SDP is the local IPv4 address of a UDP connection to the peer: the INVITE source on an inbound dialog, the request-URI host on `Dial`. SDP, Via, and Contact never carry `0.0.0.0`.

SIPp UAS check: SDP `c=` is that local IP.

## standalone

Bind local, advertise public. Advertise IP order:

1. `Config.AdvertiseIP` when it is set.
2. Otherwise a STUN Binding (RFC 8489) from the SIP UDP socket to `Config.STUNServer`.

The mapped IP becomes the advertise IP. The mapped port is recorded separately and is not assumed to equal the local port.

`PortMap: identity` advertises the local bind port. `PortMap: stun` advertises that socket's mapped port. An empty `PortMap` in standalone mode is `identity` until a story sets stun on purpose. JAR-3 tests both values explicitly.

`Start` refuses standalone when both `AdvertiseIP` and `STUNServer` are empty.

SIPp UAS check: SDP `c=` is the advertise IP.

## One Binding per socket

SIP and RTP are different sockets. Each RTP socket sends its own Binding request. The SIP result is not copied onto an RTP socket. A result from one call is not reused on the next call. RTP and RTCP are different sockets, so a call with `PortMap: stun` sends two Binding requests before it writes SDP, one from the RTP socket and one from the RTCP socket.

The SDP connection address stays the endpoint advertise IP. `PortMap: stun` puts each socket's mapped port into SDP (`m=` for RTP, `a=rtcp` when the ports are not consecutive). A Binding that fails, or that returns no XOR-MAPPED-ADDRESS, fails that setup. The call is not offered a guessed public port.

STUN is a Binding request from `pion/stun/v4` v4.0.1. Retransmission and timeout follow RFC 8489. The magic cookie `0x2112A442` is how the latch tells STUN from RTP ([latch.md](latch.md)).

## What is advertised

| Field | behind-sbc | standalone, identity | standalone, stun |
| --- | --- | --- | --- |
| Via sent-by | local IP, local SIP port | advertise IP, local SIP port | advertise IP, SIP mapped port |
| Contact host | local IP, local SIP port | advertise IP, local SIP port | advertise IP, SIP mapped port |
| SDP `c=` | local IP | advertise IP | advertise IP |
| SDP `m=` port | local RTP port | local RTP port | RTP mapped port |
| RTCP | RTP port + 1, no `a=rtcp` | RTP port + 1, no `a=rtcp` | mapped RTCP port, `a=rtcp` when not consecutive |
