package sip

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
)

func crlf(s string) string {
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func TestOfferRoundTrip(t *testing.T) {
	origin := Origin{SessionID: 42}
	addr := MediaAddr{IP: net.ParseIP("203.0.113.10"), RTPPort: 10000, RTCPPort: 10001}
	desc, raw, err := origin.Offer(addr, Config{}, DirectionSendRecv)
	if err != nil {
		t.Fatal(err)
	}
	want := crlf(`v=0
o=- 42 1 IN IP4 203.0.113.10
s=-
c=IN IP4 203.0.113.10
t=0 0
m=audio 10000 RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-15
a=ptime:20
a=sendrecv
`)
	if string(raw) != want {
		t.Fatalf("offer\n%s\nwant\n%s", raw, want)
	}
	if strings.Contains(string(raw), "a=rtcp:") || strings.Contains(string(raw), "rtcp-mux") {
		t.Fatalf("consecutive ports wrote rtcp mux or a=rtcp:\n%s", raw)
	}
	for _, banned := range []string{"opus", "SAVP", "CN/8000", "t38", "a=candidate", "a=fingerprint"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("offer contains %s", banned)
		}
	}

	parsed, err := Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := sameSDP(desc, parsed); err != nil {
		t.Fatal(err)
	}
	again, err := parsed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != want {
		t.Fatalf("re-marshal\n%s", again)
	}

	held, rawHold, err := origin.Offer(addr, Config{}, DirectionSendOnly)
	if err != nil {
		t.Fatal(err)
	}
	if held.Version != 2 || held.SessionID != 42 || held.Direction != DirectionSendOnly {
		t.Fatalf("second offer %+v", held)
	}
	if !strings.Contains(string(rawHold), "a=sendonly\r\n") || strings.Contains(string(rawHold), "a=sendrecv") {
		t.Fatalf("direction\n%s", rawHold)
	}
	heldParsed, err := Parse(string(rawHold))
	if err != nil {
		t.Fatal(err)
	}
	if err := sameSDP(held, heldParsed); err != nil {
		t.Fatal(err)
	}
}

func TestOfferNonConsecutiveRTCP(t *testing.T) {
	origin := Origin{SessionID: 7}
	addr := MediaAddr{IP: net.ParseIP("203.0.113.10"), RTPPort: 40000, RTCPPort: 42222}
	desc, raw, err := origin.Offer(addr, Config{Codecs: []Codec{CodecPCMA}, DTMF: DTMFConfig{PayloadType: 110}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !desc.RTCPExplicit || desc.RTCPPort != 42222 || desc.RTPPort != 40000 {
		t.Fatalf("ports %+v", desc)
	}
	text := string(raw)
	if !strings.Contains(text, "m=audio 40000 RTP/AVP 8 110\r\n") {
		t.Fatalf("m line\n%s", text)
	}
	if !strings.Contains(text, "a=rtpmap:8 PCMA/8000\r\n") || strings.Contains(text, "PCMU") {
		t.Fatalf("codecs\n%s", text)
	}
	if !strings.Contains(text, "a=rtpmap:110 telephone-event/8000\r\n") || !strings.Contains(text, "a=fmtp:110 0-15\r\n") {
		t.Fatalf("dtmf\n%s", text)
	}
	if !strings.Contains(text, "a=rtcp:42222\r\n") || !strings.Contains(text, "a=ptime:20\r\n") || !strings.Contains(text, "a=sendrecv\r\n") {
		t.Fatalf("attributes\n%s", text)
	}
	parsed, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if err := sameSDP(desc, parsed); err != nil {
		t.Fatal(err)
	}
}

func TestOfferRefusesUnspecified(t *testing.T) {
	origin := Origin{SessionID: 1}
	_, _, err := origin.Offer(MediaAddr{IP: net.IPv4(0, 0, 0, 0), RTPPort: 10000}, Config{}, "")
	if err == nil {
		t.Fatal("0.0.0.0 was written into SDP")
	}
	if origin.version != 0 {
		t.Fatalf("failed offer bumped version to %d", origin.version)
	}
}

func TestSIPpOfferRoundTrip(t *testing.T) {
	raw := crlf(`v=0
o=user1 53655765 2353687637 IN IP4 192.168.1.20
s=-
c=IN IP4 192.168.1.20
t=0 0
m=audio 6000 RTP/AVP 8 0 101 13
a=rtpmap:8 PCMA/8000
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=rtpmap:13 CN/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
`)
	got, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.OriginUser != "user1" || got.SessionID != 53655765 || got.Version != 2353687637 {
		t.Fatalf("origin %+v", got)
	}
	if !got.Connection.Equal(net.ParseIP("192.168.1.20")) || got.RTPPort != 6000 {
		t.Fatalf("c/m %v %d", got.Connection, got.RTPPort)
	}
	if got.RTCPExplicit || got.RTCPPort != 6001 {
		t.Fatalf("rtcp explicit %v port %d", got.RTCPExplicit, got.RTCPPort)
	}
	if got.PTime != 20 || got.Direction != DirectionSendRecv {
		t.Fatalf("ptime %d dir %s", got.PTime, got.Direction)
	}
	audio := got.Audio()
	if len(audio) != 2 || audio[0].Name != string(CodecPCMA) || audio[0].Payload != 8 || audio[1].Name != string(CodecPCMU) || audio[1].Payload != 0 {
		t.Fatalf("audio %+v", audio)
	}
	te, err := got.DTMF()
	if err != nil {
		t.Fatal(err)
	}
	if te.Payload != 101 || te.Clock != 8000 || te.FMTP != "0-16" {
		t.Fatalf("dtmf %+v", te)
	}
	for _, f := range got.Formats {
		if f.Name == "CN" || f.Payload == 13 {
			t.Fatalf("kept comfort noise %+v", got.Formats)
		}
	}

	again, err := got.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(string(again))
	if err != nil {
		t.Fatal(err)
	}
	if err := sameSDP(got, parsed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(again), "CN/8000") || strings.Contains(string(again), " 13") {
		t.Fatalf("re-marshal kept CN\n%s", again)
	}
}

func TestSIPpOfferWithRTCP(t *testing.T) {
	raw := crlf(`v=0
o=user1 53655765 2353687637 IN IP4 198.51.100.20
s=SIPp
c=IN IP4 198.51.100.8
t=0 0
m=audio 6000 RTP/AVP 0 8 101
c=IN IP4 198.51.100.20
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=ptime:20
a=rtcp:6005 IN IP4 198.51.100.30
a=sendonly
`)
	got, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Connection.Equal(net.ParseIP("198.51.100.20")) {
		t.Fatalf("connection %v", got.Connection)
	}
	if !got.RTCPExplicit || got.RTCPPort != 6005 || !got.RTCPIP.Equal(net.ParseIP("198.51.100.30")) {
		t.Fatalf("rtcp explicit %v port %d ip %v", got.RTCPExplicit, got.RTCPPort, got.RTCPIP)
	}
	if got.Direction != DirectionSendOnly || got.SessionName != "SIPp" {
		t.Fatalf("dir %s name %s", got.Direction, got.SessionName)
	}
	te, err := got.DTMF()
	if err != nil {
		t.Fatal(err)
	}
	if te.FMTP != "0-15" {
		t.Fatalf("missing fmtp became %q", te.FMTP)
	}
	stored := ""
	for _, f := range got.Formats {
		if f.Name == telephoneEvent {
			stored = f.FMTP
		}
	}
	if stored != "" {
		t.Fatalf("parser stored a default fmtp %q", stored)
	}
	again, err := got.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(again), "a=rtcp:6005 IN IP4 198.51.100.30\r\n") {
		t.Fatalf("rtcp line\n%s", again)
	}
	parsed, err := Parse(string(again))
	if err != nil {
		t.Fatal(err)
	}
	if err := sameSDP(got, parsed); err != nil {
		t.Fatal(err)
	}

	origin := Origin{SessionID: 4}
	_, answer, err := origin.Answer(MediaAddr{IP: net.ParseIP("203.0.113.10"), RTPPort: 10000}, got, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(answer), "a=fmtp:101 0-15\r\n") {
		t.Fatalf("missing fmtp was not defaulted\n%s", answer)
	}
	if origin.version != 1 {
		t.Fatalf("answer version %d", origin.version)
	}
}

func TestParseDropsOtherTelephoneEventClock(t *testing.T) {
	raw := crlf(`v=0
o=- 1 1 IN IP4 192.0.2.8
s=-
c=IN IP4 192.0.2.8
t=0 0
m=audio 49170 RTP/AVP 0 96 97
a=rtpmap:0 PCMU/8000
a=rtpmap:96 telephone-event/16000
a=rtpmap:97 telephone-event/8000
a=fmtp:97 0-15
`)
	got, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := got.DTMF(); err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(got.Formats))
	for i, f := range got.Formats {
		names[i] = f.Name
		if f.Clock != 8000 {
			t.Fatalf("kept clock %d on %+v", f.Clock, f)
		}
	}
	if strings.Join(names, ",") != "PCMU,telephone-event" {
		t.Fatalf("formats %v", got.Formats)
	}
	if _, err := (Description{}).DTMF(); !errors.Is(err, ErrDTMFNotNegotiated) {
		t.Fatalf("missing dtmf err %v", err)
	}
}

func TestAnswerSelectsFirstConfiguredCodec(t *testing.T) {
	sipp := crlf(`v=0
o=user1 53655765 2353687637 IN IP4 192.168.1.20
s=-
c=IN IP4 192.168.1.20
t=0 0
m=audio 6000 RTP/AVP 8 0 110
a=rtpmap:8 PCMA/8000
a=rtpmap:0 PCMU/8000
a=rtpmap:110 telephone-event/8000
a=fmtp:110 0-16
a=ptime:30
a=sendonly
`)
	offer, err := Parse(sipp)
	if err != nil {
		t.Fatal(err)
	}
	origin := Origin{SessionID: 99}
	addr := MediaAddr{IP: net.ParseIP("203.0.113.10"), RTPPort: 10000, RTCPPort: 10001}
	desc, raw, err := origin.Answer(addr, offer, Config{})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "m=audio 10000 RTP/AVP 0 110\r\n") {
		t.Fatalf("answer m line\n%s", text)
	}
	if strings.Contains(text, "PCMA") {
		t.Fatalf("answer kept the later codec\n%s", text)
	}
	if !strings.Contains(text, "a=rtpmap:110 telephone-event/8000\r\n") || !strings.Contains(text, "a=fmtp:110 0-16\r\n") {
		t.Fatalf("dtmf\n%s", text)
	}
	if !strings.Contains(text, "a=ptime:20\r\n") || !strings.Contains(text, "a=sendonly\r\n") {
		t.Fatalf("ptime or direction\n%s", text)
	}
	if strings.Contains(text, "a=rtcp:") {
		t.Fatalf("answer wrote a=rtcp\n%s", text)
	}
	if desc.Version != 1 || origin.version != 1 {
		t.Fatalf("version desc %d origin %d", desc.Version, origin.version)
	}
	if _, _, err := origin.Answer(addr, offer, Config{}); err != nil {
		t.Fatal(err)
	}
	if origin.version != 1 {
		t.Fatalf("answer bumped version to %d", origin.version)
	}
	next, _, err := origin.Offer(addr, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.Version != 2 || next.SessionID != 99 {
		t.Fatalf("offer after answer %+v", next)
	}

	onlyPCMA := offer
	onlyPCMA.Formats = []Format{{Payload: 8, Name: string(CodecPCMA), Clock: 8000}}
	pcma, rawPCMA, err := origin.Answer(addr, onlyPCMA, Config{Codecs: []Codec{CodecPCMU, CodecPCMA}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pcma.Audio()) != 1 || pcma.Audio()[0].Payload != 8 {
		t.Fatalf("pcma answer %+v", pcma.Audio())
	}
	if _, err := pcma.DTMF(); !errors.Is(err, ErrDTMFNotNegotiated) {
		t.Fatalf("dtmf err %v", err)
	}
	if strings.Contains(string(rawPCMA), "telephone-event") {
		t.Fatalf("answer invented telephone-event\n%s", rawPCMA)
	}

	_, _, err = origin.Answer(addr, Description{Connection: net.ParseIP("192.0.2.1"), RTPPort: 6000}, Config{})
	if !errors.Is(err, ErrNoCommonCodec) {
		t.Fatalf("err %v", err)
	}
}

func TestAcceptAnswerUsesPeerPayload(t *testing.T) {
	origin := Origin{SessionID: 5}
	addr := MediaAddr{IP: net.ParseIP("203.0.113.10"), RTPPort: 10000}
	offered, _, err := origin.Offer(addr, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	answer, err := Parse(crlf(`v=0
o=user1 11 4 IN IP4 198.51.100.8
s=-
c=IN IP4 198.51.100.8
t=0 0
m=audio 49170 RTP/AVP 8 97
a=rtpmap:8 PCMA/8000
a=rtpmap:97 telephone-event/8000
a=fmtp:97 0-15
a=ptime:20
a=recvonly
a=rtcp:49180
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := AcceptAnswer(offered, answer)
	if err != nil {
		t.Fatal(err)
	}
	if got.Audio.Name != string(CodecPCMA) || got.Audio.Payload != 8 {
		t.Fatalf("audio %+v", got.Audio)
	}
	if got.DTMF == nil || got.DTMF.Payload != 97 || got.DTMF.FMTP != "0-15" {
		t.Fatalf("dtmf %+v", got.DTMF)
	}
	if !got.Connection.Equal(net.ParseIP("198.51.100.8")) || got.RTPPort != 49170 {
		t.Fatalf("target %v %d", got.Connection, got.RTPPort)
	}
	if !got.RTCPExplicit || got.RTCPPort != 49180 {
		t.Fatalf("rtcp %+v", got)
	}
	if got.Direction != DirectionRecvOnly || got.SessionID != 11 || got.Version != 4 {
		t.Fatalf("peer origin %+v", got)
	}

	neither, err := Parse(crlf(`v=0
o=- 1 1 IN IP4 192.0.2.9
s=-
c=IN IP4 192.0.2.9
t=0 0
m=audio 6002 RTP/AVP 96
a=rtpmap:96 opus/48000/2
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptAnswer(offered, neither); !errors.Is(err, ErrNoCommonCodec) {
		t.Fatalf("err %v", err)
	}
}

func TestOfferFromAllocatedPair(t *testing.T) {
	cfg := baseConfig()
	ep := startEP(t, cfg)
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	origin, err := NewOrigin()
	if err != nil {
		t.Fatal(err)
	}
	desc, raw, err := origin.Offer(MediaAddr{
		IP:       pair.AdvertiseIP,
		RTPPort:  pair.AdvertiseRTPPort,
		RTCPPort: pair.AdvertiseRTCPPort,
	}, ep.cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !desc.Connection.Equal(net.ParseIP("127.0.0.1")) || desc.RTPPort != pair.LocalRTPPort {
		t.Fatalf("sdp %v %d local %d", desc.Connection, desc.RTPPort, pair.LocalRTPPort)
	}
	if desc.RTCPExplicit || strings.Contains(string(raw), "a=rtcp:") {
		t.Fatalf("identity offer wrote a=rtcp\n%s", raw)
	}
	if pair.LocalRTPPort%2 != 0 || pair.LocalRTCPPort != pair.LocalRTPPort+1 {
		t.Fatalf("ports %d %d", pair.LocalRTPPort, pair.LocalRTCPPort)
	}
}

func TestOfferFromSTUNPairWritesRTCP(t *testing.T) {
	box := newSTUNBox(t)
	cfg := baseConfig()
	cfg.NATMode = NATStandalone
	cfg.AdvertiseIP = "203.0.113.10"
	cfg.PortMap = PortMapSTUN
	cfg.STUNServer = box.addr()
	ep := startEP(t, cfg)
	pair, err := ep.AllocateRTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	origin := Origin{SessionID: 3}
	_, raw, err := origin.Offer(MediaAddr{
		IP:       pair.AdvertiseIP,
		RTPPort:  pair.AdvertiseRTPPort,
		RTCPPort: pair.AdvertiseRTCPPort,
	}, ep.cfg, DirectionSendRecv)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "c=IN IP4 203.0.113.10\r\n") {
		t.Fatalf("c line\n%s", text)
	}
	if strings.Contains(text, "203.0.113.50") {
		t.Fatalf("sdp used the mapped IP\n%s", text)
	}
	m := "m=audio " + strconv.Itoa(pair.AdvertiseRTPPort) + " RTP/AVP 0 8 101\r\n"
	rtcp := "a=rtcp:" + strconv.Itoa(pair.AdvertiseRTCPPort) + "\r\n"
	if !strings.Contains(text, m) || !strings.Contains(text, rtcp) {
		t.Fatalf("ports rtp %d rtcp %d\n%s", pair.AdvertiseRTPPort, pair.AdvertiseRTCPPort, text)
	}
}

func sameSDP(a, b Description) error {
	if a.OriginUser != b.OriginUser || a.SessionID != b.SessionID || a.Version != b.Version {
		return errors.New("origin mismatch")
	}
	if a.SessionName != b.SessionName || a.RTPPort != b.RTPPort || a.RTCPPort != b.RTCPPort || a.RTCPExplicit != b.RTCPExplicit {
		return errors.New("session mismatch")
	}
	if a.PTime != b.PTime || a.Direction != b.Direction {
		return errors.New("attribute mismatch")
	}
	if !sameIP(a.OriginIP, b.OriginIP) || !sameIP(a.Connection, b.Connection) || !sameIP(a.RTCPIP, b.RTCPIP) {
		return errors.New("address mismatch")
	}
	if len(a.Formats) != len(b.Formats) {
		return errors.New("format count mismatch")
	}
	for i := range a.Formats {
		af, bf := a.Formats[i], b.Formats[i]
		if af.Payload != bf.Payload || af.Name != bf.Name || af.Clock != bf.Clock || af.FMTP != bf.FMTP {
			return errors.New("format mismatch")
		}
	}
	return nil
}

func sameIP(a, b net.IP) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == 0 && len(b) == 0
	}
	return a.Equal(b)
}
