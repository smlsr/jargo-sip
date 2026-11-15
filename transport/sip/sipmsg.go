package sip

import (
	"net"
	"strings"

	"github.com/emiago/sipgo/sip"
)

const allowMethods = "INVITE, ACK, CANCEL, BYE, UPDATE, PRACK, REFER, NOTIFY, OPTIONS"

func (e *Endpoint) supportedValue() string {
	return "100rel"
}

func (e *Endpoint) supportedHeader() sip.Header {
	return sip.NewHeader("Supported", e.supportedValue())
}

func (e *Endpoint) allowHeader() sip.Header {
	return sip.NewHeader("Allow", allowMethods)
}

// decorate adds the headers every response we build carries.
func (e *Endpoint) decorate(res *sip.Response, sdp bool) {
	res.AppendHeader(e.allowHeader())
	res.AppendHeader(e.supportedHeader())
	res.AppendHeader(sip.HeaderClone(&e.contact))
	if sdp {
		ct := sip.ContentTypeHeader("application/sdp")
		res.AppendHeader(&ct)
	}
}

func (e *Endpoint) respond(tx sip.ServerTransaction, req *sip.Request, code int, reason string, body []byte, extra ...sip.Header) error {
	res := sip.NewResponseFromRequest(req, code, reason, body)
	e.decorate(res, len(body) > 0)
	for _, h := range extra {
		if h != nil {
			res.AppendHeader(h)
		}
	}
	return tx.Respond(res)
}

func (e *Endpoint) sessionRespond(sess sipgoDialog, code int, reason string, body []byte, extra ...sip.Header) error {
	hs := []sip.Header{e.allowHeader(), e.supportedHeader()}
	if len(body) > 0 {
		ct := sip.ContentTypeHeader("application/sdp")
		hs = append(hs, &ct)
	}
	for _, h := range extra {
		if h != nil {
			hs = append(hs, h)
		}
	}
	return sess.Respond(code, reason, body, hs...)
}

// sipgoDialog is the subset of DialogServerSession.Respond we call.
// The real type is *sipgo.DialogServerSession. This alias keeps sipmsg free
// of a second import cycle in the signature used by inbound.go.
type sipgoDialog interface {
	Respond(statusCode int, reason string, body []byte, headers ...sip.Header) error
}

func (e *Endpoint) via() *sip.ViaHeader {
	via := &sip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       "UDP",
		Host:            e.contact.Address.Host,
		Port:            e.contact.Address.Port,
	}
	via.Params = sip.NewParams()
	via.Params.Add("branch", sip.GenerateBranch())
	return via
}

func (e *Endpoint) mediaAddr(p *RTPPair) MediaAddr {
	return MediaAddr{
		IP:       cloneIP(p.AdvertiseIP),
		RTPPort:  p.AdvertiseRTPPort,
		RTCPPort: p.AdvertiseRTCPPort,
	}
}

func hasToken(msg sip.Message, header, token string) bool {
	if msg == nil {
		return false
	}
	for _, h := range msg.GetHeaders(header) {
		for _, part := range strings.Split(h.Value(), ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func wants100rel(req *sip.Request) bool {
	return hasToken(req, "Supported", "100rel") || hasToken(req, "Require", "100rel")
}

func contactOf(msg sip.Message) *sip.ContactHeader {
	switch m := msg.(type) {
	case *sip.Request:
		return m.Contact()
	case *sip.Response:
		return m.Contact()
	default:
		return nil
	}
}

func callIDOf(req *sip.Request) string {
	if req == nil || req.CallID() == nil {
		return ""
	}
	return req.CallID().Value()
}

func inviteAllowed(source string, allow []string) bool {
	host, _, err := net.SplitHostPort(source)
	if err != nil {
		host = source
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, raw := range allow {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			_, network, err := net.ParseCIDR(item)
			if err == nil && network.Contains(ip) {
				return true
			}
			continue
		}
		if other := net.ParseIP(item); other != nil && other.Equal(ip) {
			return true
		}
	}
	return false
}

func phrase(code int) string {
	switch code {
	case sip.StatusTrying:
		return "Trying"
	case sip.StatusRinging:
		return "Ringing"
	case sip.StatusSessionInProgress:
		return "Session Progress"
	case sip.StatusOK:
		return "OK"
	case sip.StatusAccepted:
		return "Accepted"
	case sip.StatusBadRequest:
		return "Bad Request"
	case sip.StatusUnauthorized:
		return "Unauthorized"
	case sip.StatusForbidden:
		return "Forbidden"
	case sip.StatusProxyAuthRequired:
		return "Proxy Authentication Required"
	case sip.StatusRequestTimeout:
		return "Request Timeout"
	case sip.StatusBusyHere:
		return "Busy Here"
	case sip.StatusCallTransactionDoesNotExists:
		return "Call/Transaction Does Not Exist"
	case sip.StatusRequestTerminated:
		return "Request Terminated"
	case sip.StatusNotAcceptableHere:
		return "Not Acceptable Here"
	case 489:
		return "Bad Event"
	case sip.StatusRequestPending:
		return "Request Pending"
	case sip.StatusInternalServerError:
		return "Server Internal Error"
	case sip.StatusServiceUnavailable:
		return "Service Unavailable"
	default:
		return "Response"
	}
}
