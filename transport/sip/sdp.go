package sip

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	psdp "github.com/pion/sdp/v3"
)

const telephoneEvent = "telephone-event"

const (
	// DirectionSendRecv is a=sendrecv.
	DirectionSendRecv Direction = "sendrecv"
	// DirectionSendOnly is a=sendonly.
	DirectionSendOnly Direction = "sendonly"
	// DirectionRecvOnly is a=recvonly.
	DirectionRecvOnly Direction = "recvonly"
	// DirectionInactive is a=inactive.
	DirectionInactive Direction = "inactive"
)

// Direction is an SDP direction attribute. Empty means the description did not carry one.
type Direction string

var (
	// ErrNoCommonCodec is returned when an offer or answer has neither PCMU nor PCMA
	// in common with the codecs we are willing to use.
	ErrNoCommonCodec = errors.New("no common audio codec")
	// ErrDTMFNotNegotiated is returned when telephone-event/8000 was not negotiated.
	// WriteDTMF returns this and leaves the call up.
	ErrDTMFNotNegotiated = errors.New("telephone-event was not negotiated")
)

// Format is one payload kept from an audio m= line.
// PCMU, PCMA, and telephone-event/8000 are the only names stored.
type Format struct {
	Payload int
	Name    string
	Clock   int
	FMTP    string
}

// Description is the SDP session this package reads and writes.
type Description struct {
	OriginUser   string
	SessionID    uint64
	Version      uint64
	OriginIP     net.IP
	SessionName  string
	Connection   net.IP
	RTPPort      int
	RTCPPort     int
	RTCPExplicit bool
	RTCPIP       net.IP
	Formats      []Format
	PTime        int
	Direction    Direction
}

// MediaAddr is the address written into an offer or answer we send.
// RTCPPort 0 means the RTP port plus 1, and a=rtcp is omitted.
type MediaAddr struct {
	IP       net.IP
	RTPPort  int
	RTCPPort int
}

// Negotiated is the media target taken from an answer to an offer we sent.
type Negotiated struct {
	Audio        Format
	DTMF         *Format
	Connection   net.IP
	RTPPort      int
	RTCPPort     int
	RTCPExplicit bool
	RTCPIP       net.IP
	Direction    Direction
	SessionID    uint64
	Version      uint64
	OriginIP     net.IP
}

// Origin is the o= identity of one call. The session id is chosen once.
// The version starts at 1 on the first SDP we send and increases by 1
// on every offer after that. An answer does not increase it.
type Origin struct {
	SessionID uint64
	version   uint64
}

// NewOrigin chooses a random non-zero session id.
func NewOrigin() (Origin, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return Origin{}, err
	}
	id := binary.BigEndian.Uint64(buf[:])
	if id == 0 {
		id = 1
	}
	return Origin{SessionID: id}, nil
}

// Offer builds the SDP we send. Codec order follows cfg. The default is
// PCMU, PCMA, then telephone-event. a=rtcp is included only when the RTP
// and RTCP ports are not consecutive.
func (o *Origin) Offer(addr MediaAddr, cfg Config, dir Direction) (Description, []byte, error) {
	if o == nil || o.SessionID == 0 {
		return Description{}, nil, errors.New("sdp session id is missing")
	}
	formats, err := offerFormats(cfg)
	if err != nil {
		return Description{}, nil, err
	}
	dir, err = normalizeDirection(dir)
	if err != nil {
		return Description{}, nil, err
	}
	base, err := localBase(addr)
	if err != nil {
		return Description{}, nil, err
	}
	ver := o.version + 1
	if o.version == 0 {
		ver = 1
	}
	desc := base
	desc.OriginUser = "-"
	desc.SessionID = o.SessionID
	desc.Version = ver
	desc.SessionName = "-"
	desc.Formats = formats
	desc.PTime = 20
	desc.Direction = dir
	raw, err := desc.Marshal()
	if err != nil {
		return Description{}, nil, err
	}
	o.version = ver
	return desc, raw, nil
}

// Answer builds the SDP we send for an inbound offer.
// The direction attribute copies the offer. The version is not increased
// when this call already sent an offer.
func (o *Origin) Answer(addr MediaAddr, offer Description, cfg Config) (Description, []byte, error) {
	if o == nil || o.SessionID == 0 {
		return Description{}, nil, errors.New("sdp session id is missing")
	}
	audio, err := chooseAudio(offer, cfg)
	if err != nil {
		return Description{}, nil, err
	}
	formats := []Format{audio}
	if te, derr := offer.DTMF(); derr == nil {
		formats = append(formats, te)
	}
	dir := offer.Direction
	if dir == "" {
		dir = DirectionSendRecv
	}
	dir, err = normalizeDirection(dir)
	if err != nil {
		return Description{}, nil, err
	}
	base, err := localBase(addr)
	if err != nil {
		return Description{}, nil, err
	}
	ver := o.version
	if ver == 0 {
		ver = 1
	}
	desc := base
	desc.OriginUser = "-"
	desc.SessionID = o.SessionID
	desc.Version = ver
	desc.SessionName = "-"
	desc.Formats = formats
	desc.PTime = 20
	desc.Direction = dir
	raw, err := desc.Marshal()
	if err != nil {
		return Description{}, nil, err
	}
	if o.version == 0 {
		o.version = ver
	}
	return desc, raw, nil
}

// Audio returns PCMU and PCMA formats in m= order.
func (d Description) Audio() []Format {
	var out []Format
	for _, f := range d.Formats {
		if f.Name == string(CodecPCMU) || f.Name == string(CodecPCMA) {
			out = append(out, f)
		}
	}
	return out
}

// DTMF returns telephone-event/8000. A missing fmtp is 0-15.
// A missing telephone-event line returns ErrDTMFNotNegotiated.
func (d Description) DTMF() (Format, error) {
	for _, f := range d.Formats {
		if f.Name == telephoneEvent && f.Clock == 8000 {
			if f.FMTP == "" {
				f.FMTP = "0-15"
			}
			return f, nil
		}
	}
	return Format{}, ErrDTMFNotNegotiated
}

// AcceptAnswer reads an answer to an offer we sent.
// The audio payload type and the telephone-event payload type are the ones in the answer.
func AcceptAnswer(offered, answer Description) (Negotiated, error) {
	if answer.RTPPort < 1 || answer.RTPPort > 65535 {
		return Negotiated{}, errors.New("sdp media port is closed")
	}
	if len(answer.Connection) == 0 {
		return Negotiated{}, errors.New("sdp connection address is missing")
	}
	wanted := map[string]bool{}
	for _, f := range offered.Audio() {
		wanted[f.Name] = true
	}
	var audio Format
	found := false
	for _, f := range answer.Audio() {
		if wanted[f.Name] {
			audio = f
			found = true
			break
		}
	}
	if !found {
		return Negotiated{}, ErrNoCommonCodec
	}
	var dtmf *Format
	if _, err := offered.DTMF(); err == nil {
		if te, derr := answer.DTMF(); derr == nil {
			dtmf = &te
		}
	}
	dir := answer.Direction
	if dir == "" {
		dir = DirectionSendRecv
	}
	return Negotiated{
		Audio:        audio,
		DTMF:         dtmf,
		Connection:   cloneIP(answer.Connection),
		RTPPort:      answer.RTPPort,
		RTCPPort:     answer.RTCPPort,
		RTCPExplicit: answer.RTCPExplicit,
		RTCPIP:       cloneIP(answer.RTCPIP),
		Direction:    dir,
		SessionID:    answer.SessionID,
		Version:      answer.Version,
		OriginIP:     cloneIP(answer.OriginIP),
	}, nil
}

// Marshal writes description in the offer layout this package sends.
func (d Description) Marshal() ([]byte, error) {
	if len(d.Formats) == 0 {
		return nil, errors.New("sdp has no media format")
	}
	if d.RTPPort < 1 || d.RTPPort > 65535 {
		return nil, fmt.Errorf("RTP port %d is out of range", d.RTPPort)
	}
	originIP, err := ipv4(d.OriginIP)
	if err != nil {
		return nil, err
	}
	connIP, err := ipv4(d.Connection)
	if err != nil {
		return nil, err
	}
	user := d.OriginUser
	if user == "" {
		user = "-"
	}
	name := d.SessionName
	if name == "" {
		name = "-"
	}
	if d.Direction != "" {
		if _, err := normalizeDirection(d.Direction); err != nil {
			return nil, err
		}
	}
	explicit := d.RTCPExplicit || (d.RTCPPort != 0 && d.RTCPPort != d.RTPPort+1)
	if explicit {
		if d.RTCPPort < 1 || d.RTCPPort > 65535 {
			return nil, fmt.Errorf("RTCP port %d is out of range", d.RTCPPort)
		}
	}

	formats := make([]string, len(d.Formats))
	md := &psdp.MediaDescription{
		MediaName: psdp.MediaName{
			Media:   "audio",
			Port:    psdp.RangedPort{Value: d.RTPPort},
			Protos:  []string{"RTP", "AVP"},
			Formats: formats,
		},
	}
	for i, f := range d.Formats {
		formats[i] = strconv.Itoa(f.Payload)
		md.WithValueAttribute("rtpmap", fmt.Sprintf("%d %s/%d", f.Payload, f.Name, f.Clock))
		if f.FMTP != "" {
			md.WithValueAttribute("fmtp", fmt.Sprintf("%d %s", f.Payload, f.FMTP))
		}
	}
	if explicit {
		val := strconv.Itoa(d.RTCPPort)
		if len(d.RTCPIP) > 0 {
			rip, rerr := ipv4(d.RTCPIP)
			if rerr != nil {
				return nil, rerr
			}
			val += " IN IP4 " + rip.String()
		}
		md.WithValueAttribute("rtcp", val)
	}
	if d.PTime > 0 {
		md.WithValueAttribute("ptime", strconv.Itoa(d.PTime))
	}
	if d.Direction != "" {
		md.WithPropertyAttribute(string(d.Direction))
	}

	sd := &psdp.SessionDescription{
		Version: 0,
		Origin: psdp.Origin{
			Username:       user,
			SessionID:      d.SessionID,
			SessionVersion: d.Version,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: originIP.String(),
		},
		SessionName:           psdp.SessionName(name),
		ConnectionInformation: connInfo(connIP),
		TimeDescriptions: []psdp.TimeDescription{{
			Timing: psdp.Timing{StartTime: 0, StopTime: 0},
		}},
		MediaDescriptions: []*psdp.MediaDescription{md},
	}
	return sd.Marshal()
}

// Parse reads an offer or an answer. PCMU/8000, PCMA/8000, and
// telephone-event/8000 are kept. Other payload types are ignored.
// A missing a=rtcp means RTCP is the RTP port plus 1.
func Parse(raw string) (Description, error) {
	// pion's parser returns EOF when the description does not end in a newline.
	if raw != "" && !strings.HasSuffix(raw, "\n") {
		raw += "\n"
	}
	var sd psdp.SessionDescription
	if err := sd.UnmarshalString(raw); err != nil {
		return Description{}, err
	}
	originIP, err := parseIPv4(sd.Origin.UnicastAddress)
	if err != nil {
		return Description{}, fmt.Errorf("sdp origin: %w", err)
	}
	var audio *psdp.MediaDescription
	for _, md := range sd.MediaDescriptions {
		if md != nil && md.MediaName.Media == "audio" {
			audio = md
			break
		}
	}
	if audio == nil {
		return Description{}, errors.New("sdp has no audio")
	}
	if strings.Join(audio.MediaName.Protos, "/") != "RTP/AVP" {
		return Description{}, errors.New("sdp audio is not RTP/AVP")
	}
	if audio.MediaName.Port.Range != nil {
		return Description{}, errors.New("sdp port ranges are not supported")
	}
	conn, err := connectionIP(audio.ConnectionInformation, sd.ConnectionInformation)
	if err != nil {
		return Description{}, err
	}
	formats, err := keepFormats(audio)
	if err != nil {
		return Description{}, err
	}
	rtp := audio.MediaName.Port.Value
	rtcp := 0
	explicit := false
	var rtcpIP net.IP
	if port, ip, ok := rtcpAttr(audio.Attributes); ok {
		rtcp = port
		explicit = true
		rtcpIP = ip
	} else if rtp > 0 && rtp < 65535 {
		rtcp = rtp + 1
	}
	dir, err := directionOf(audio.Attributes, sd.Attributes)
	if err != nil {
		return Description{}, err
	}
	ptime := ptimeOf(audio.Attributes, sd.Attributes)
	return Description{
		OriginUser:   sd.Origin.Username,
		SessionID:    sd.Origin.SessionID,
		Version:      sd.Origin.SessionVersion,
		OriginIP:     originIP,
		SessionName:  string(sd.SessionName),
		Connection:   conn,
		RTPPort:      rtp,
		RTCPPort:     rtcp,
		RTCPExplicit: explicit,
		RTCPIP:       rtcpIP,
		Formats:      formats,
		PTime:        ptime,
		Direction:    dir,
	}, nil
}

func localBase(addr MediaAddr) (Description, error) {
	ip, err := ipv4(addr.IP)
	if err != nil {
		return Description{}, err
	}
	rtcp, explicit, err := resolveRTCP(addr.RTPPort, addr.RTCPPort)
	if err != nil {
		return Description{}, err
	}
	return Description{
		OriginIP:     ip,
		Connection:   cloneIP(ip),
		RTPPort:      addr.RTPPort,
		RTCPPort:     rtcp,
		RTCPExplicit: explicit,
	}, nil
}

func resolveRTCP(rtp, rtcp int) (int, bool, error) {
	if rtp < 1 || rtp > 65535 {
		return 0, false, fmt.Errorf("RTP port %d is out of range", rtp)
	}
	if rtcp == 0 || rtcp == rtp+1 {
		if rtp == 65535 {
			return 0, false, errors.New("RTCP port is out of range")
		}
		return rtp + 1, false, nil
	}
	if rtcp < 1 || rtcp > 65535 {
		return 0, false, fmt.Errorf("RTCP port %d is out of range", rtcp)
	}
	if rtcp == rtp {
		return 0, false, errors.New("RTCP port equals the RTP port")
	}
	return rtcp, true, nil
}

func offerFormats(cfg Config) ([]Format, error) {
	codecs, err := canonicalCodecs(cfg)
	if err != nil {
		return nil, err
	}
	pt, err := dtmfPayload(cfg)
	if err != nil {
		return nil, err
	}
	out := make([]Format, 0, len(codecs)+1)
	for _, c := range codecs {
		payload, perr := staticPayload(c)
		if perr != nil {
			return nil, perr
		}
		if payload == pt {
			return nil, fmt.Errorf("DTMF payload %d collides with %s", pt, c)
		}
		out = append(out, Format{Payload: payload, Name: string(c), Clock: 8000})
	}
	out = append(out, Format{Payload: pt, Name: telephoneEvent, Clock: 8000, FMTP: "0-15"})
	return out, nil
}

func canonicalCodecs(cfg Config) ([]Codec, error) {
	list := cfg.Codecs
	if len(list) == 0 {
		list = []Codec{CodecPCMU, CodecPCMA}
	}
	out := make([]Codec, 0, len(list))
	seen := map[Codec]bool{}
	for _, c := range list {
		switch c {
		case CodecPCMU, CodecPCMA:
		default:
			return nil, fmt.Errorf("codec %q is unknown", c)
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no audio codec is configured")
	}
	return out, nil
}

func staticPayload(c Codec) (int, error) {
	switch c {
	case CodecPCMU:
		return 0, nil
	case CodecPCMA:
		return 8, nil
	default:
		return 0, fmt.Errorf("codec %q is unknown", c)
	}
}

func dtmfPayload(cfg Config) (int, error) {
	pt := cfg.DTMF.PayloadType
	if pt == 0 {
		pt = 101
	}
	if pt < 0 || pt > 127 {
		return 0, fmt.Errorf("DTMF payload %d is out of range", pt)
	}
	return pt, nil
}

func chooseAudio(offer Description, cfg Config) (Format, error) {
	codecs, err := canonicalCodecs(cfg)
	if err != nil {
		return Format{}, err
	}
	for _, want := range codecs {
		for _, f := range offer.Audio() {
			if f.Name == string(want) {
				return f, nil
			}
		}
	}
	return Format{}, ErrNoCommonCodec
}

func normalizeDirection(d Direction) (Direction, error) {
	if d == "" {
		return DirectionSendRecv, nil
	}
	switch d {
	case DirectionSendRecv, DirectionSendOnly, DirectionRecvOnly, DirectionInactive:
		return d, nil
	default:
		return "", fmt.Errorf("direction %q is unknown", d)
	}
}

func ipv4(ip net.IP) (net.IP, error) {
	if len(ip) == 0 {
		return nil, errors.New("sdp address is missing")
	}
	v4 := ip.To4()
	if v4 == nil {
		return nil, fmt.Errorf("sdp address %s is not IPv4", ip)
	}
	if v4.IsUnspecified() {
		return nil, errors.New("sdp address is 0.0.0.0")
	}
	if v4.IsMulticast() {
		return nil, errors.New("sdp address is multicast")
	}
	return cloneIP(v4), nil
}

func parseIPv4(s string) (net.IP, error) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("sdp address %q is not an IP", s)
	}
	v4 := ip.To4()
	if v4 == nil {
		return nil, fmt.Errorf("sdp address %q is not IPv4", s)
	}
	return cloneIP(v4), nil
}

func connInfo(ip net.IP) *psdp.ConnectionInformation {
	return &psdp.ConnectionInformation{
		NetworkType: "IN",
		AddressType: "IP4",
		Address:     &psdp.Address{Address: ip.String()},
	}
}

func connectionIP(media, session *psdp.ConnectionInformation) (net.IP, error) {
	for _, c := range []*psdp.ConnectionInformation{media, session} {
		if c == nil || c.Address == nil || strings.TrimSpace(c.Address.Address) == "" {
			continue
		}
		if c.AddressType != "" && c.AddressType != "IP4" {
			return nil, fmt.Errorf("sdp address type %s is not IP4", c.AddressType)
		}
		ip, err := parseIPv4(c.Address.Address)
		if err != nil {
			return nil, err
		}
		return ip, nil
	}
	return nil, errors.New("sdp connection address is missing")
}

func keepFormats(md *psdp.MediaDescription) ([]Format, error) {
	rtpmaps := map[int]mappedCodec{}
	fmtps := map[int]string{}
	for _, a := range md.Attributes {
		switch strings.ToLower(a.Key) {
		case "rtpmap":
			pt, mapped, err := parseRtpmap(a.Value)
			if err != nil {
				return nil, err
			}
			rtpmaps[pt] = mapped
		case "fmtp":
			pt, rest, err := parseFmtp(a.Value)
			if err != nil {
				return nil, err
			}
			fmtps[pt] = rest
		}
	}
	var out []Format
	for _, raw := range md.MediaName.Formats {
		pt, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("sdp format %q is not a payload type", raw)
		}
		mapped, ok := rtpmaps[pt]
		if !ok {
			mapped, ok = staticCodec(pt)
		}
		if !ok || !wantCodec(mapped) {
			continue
		}
		out = append(out, Format{
			Payload: pt,
			Name:    mapped.name,
			Clock:   mapped.clock,
			FMTP:    fmtps[pt],
		})
	}
	return out, nil
}

type mappedCodec struct {
	name    string
	clock   int
	channel int
}

func staticCodec(pt int) (mappedCodec, bool) {
	switch pt {
	case 0:
		return mappedCodec{name: string(CodecPCMU), clock: 8000, channel: 1}, true
	case 8:
		return mappedCodec{name: string(CodecPCMA), clock: 8000, channel: 1}, true
	default:
		return mappedCodec{}, false
	}
}

func wantCodec(m mappedCodec) bool {
	if m.channel != 0 && m.channel != 1 {
		return false
	}
	if m.clock != 8000 {
		return false
	}
	switch strings.ToUpper(m.name) {
	case string(CodecPCMU), string(CodecPCMA):
		return true
	}
	return strings.EqualFold(m.name, telephoneEvent)
}

func parseRtpmap(v string) (int, mappedCodec, error) {
	fields := strings.Fields(v)
	if len(fields) != 2 {
		return 0, mappedCodec{}, fmt.Errorf("sdp rtpmap %q is invalid", v)
	}
	pt, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, mappedCodec{}, fmt.Errorf("sdp rtpmap %q is invalid", v)
	}
	parts := strings.Split(fields[1], "/")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, mappedCodec{}, fmt.Errorf("sdp rtpmap %q is invalid", v)
	}
	clock, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, mappedCodec{}, fmt.Errorf("sdp rtpmap %q is invalid", v)
	}
	ch := 1
	if len(parts) == 3 {
		ch, err = strconv.Atoi(parts[2])
		if err != nil {
			return 0, mappedCodec{}, fmt.Errorf("sdp rtpmap %q is invalid", v)
		}
	}
	name := parts[0]
	switch strings.ToUpper(name) {
	case string(CodecPCMU):
		name = string(CodecPCMU)
	case string(CodecPCMA):
		name = string(CodecPCMA)
	default:
		if strings.EqualFold(name, telephoneEvent) {
			name = telephoneEvent
		}
	}
	return pt, mappedCodec{name: name, clock: clock, channel: ch}, nil
}

func parseFmtp(v string) (int, string, error) {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return 0, "", errors.New("sdp fmtp is empty")
	}
	pt, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, "", fmt.Errorf("sdp fmtp %q is invalid", v)
	}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), fields[0]))
	return pt, rest, nil
}

func rtcpAttr(attrs []psdp.Attribute) (int, net.IP, bool) {
	for _, a := range attrs {
		if strings.ToLower(a.Key) != "rtcp" {
			continue
		}
		fields := strings.Fields(a.Value)
		if len(fields) == 0 {
			return 0, nil, false
		}
		port, err := strconv.Atoi(fields[0])
		if err != nil || port < 0 || port > 65535 {
			return 0, nil, false
		}
		var ip net.IP
		if len(fields) >= 4 {
			parsed, perr := parseIPv4(fields[3])
			if perr != nil {
				return 0, nil, false
			}
			ip = parsed
		}
		return port, ip, true
	}
	return 0, nil, false
}

func directionOf(media, session []psdp.Attribute) (Direction, error) {
	if d, ok, err := lastDirection(media); ok || err != nil {
		return d, err
	}
	d, _, err := lastDirection(session)
	return d, err
}

func lastDirection(attrs []psdp.Attribute) (Direction, bool, error) {
	var dir Direction
	found := false
	for _, a := range attrs {
		key := strings.ToLower(a.Key)
		switch Direction(key) {
		case DirectionSendRecv, DirectionSendOnly, DirectionRecvOnly, DirectionInactive:
			if a.Value != "" {
				return "", false, fmt.Errorf("direction %q is unknown", a.Key+":"+a.Value)
			}
			dir = Direction(key)
			found = true
		}
	}
	return dir, found, nil
}

func ptimeOf(media, session []psdp.Attribute) int {
	if n, ok := firstPTime(media); ok {
		return n
	}
	n, _ := firstPTime(session)
	return n
}

func firstPTime(attrs []psdp.Attribute) (int, bool) {
	for _, a := range attrs {
		if strings.ToLower(a.Key) != "ptime" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(a.Value))
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
