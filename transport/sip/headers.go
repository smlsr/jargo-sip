package sip

import (
	"strings"
	"sync"

	"github.com/emiago/sipgo/sip"
)

// Header is one SIP header field. Names compare case-insensitively.
type Header struct {
	Name  string
	Value string
}

// headerState is the caller headers for one dialog or the endpoint.
// out is applied to later requests. received is the messages that arrived,
// newest last.
type headerState struct {
	mu       sync.Mutex
	out      []Header
	received [][]Header
}

var (
	dialogHeaders   sync.Map // *dialog -> *headerState
	endpointHeaders sync.Map // *Endpoint -> *headerState
)

func dialogState(d *dialog) *headerState {
	if d == nil {
		return &headerState{}
	}
	if v, ok := dialogHeaders.Load(d); ok {
		return v.(*headerState)
	}
	st := &headerState{}
	actual, _ := dialogHeaders.LoadOrStore(d, st)
	return actual.(*headerState)
}

func endpointState(e *Endpoint) *headerState {
	if e == nil {
		return &headerState{}
	}
	if v, ok := endpointHeaders.Load(e); ok {
		return v.(*headerState)
	}
	st := &headerState{}
	actual, _ := endpointHeaders.LoadOrStore(e, st)
	return actual.(*headerState)
}

func releaseDialogHeaders(d *dialog) {
	if d == nil {
		return
	}
	dialogHeaders.Delete(d)
}

// ownedHeader is a hop-by-hop header this stack writes. A caller value
// must not replace it.
func ownedHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "via", "contact", "cseq", "call-id", "from", "to", "max-forwards", "content-length", "route", "record-route":
		return true
	default:
		return false
	}
}

func (st *headerState) set(name, value string) {
	name = strings.TrimSpace(name)
	if name == "" || ownedHeader(name) {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := range st.out {
		if strings.EqualFold(st.out[i].Name, name) {
			st.out[i] = Header{Name: name, Value: value}
			return
		}
	}
	st.out = append(st.out, Header{Name: name, Value: value})
}

func (st *headerState) outbound() []Header {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Header, len(st.out))
	copy(out, st.out)
	return out
}

func (st *headerState) note(hs []Header) {
	if len(hs) == 0 {
		return
	}
	cp := make([]Header, len(hs))
	copy(cp, hs)
	st.mu.Lock()
	st.received = append(st.received, cp)
	if len(st.received) > 64 {
		st.received = st.received[len(st.received)-64:]
	}
	st.mu.Unlock()
}

func (st *headerState) get(name string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := len(st.received) - 1; i >= 0; i-- {
		for _, h := range st.received[i] {
			if strings.EqualFold(h.Name, name) {
				return h.Value, true
			}
		}
	}
	return "", false
}

// SetHeader stores a caller header on this call. An owned hop-by-hop name
// is refused and not stored. The header is applied to later requests this
// call sends. The name match is case-insensitive.
func (c *Call) SetHeader(name, value string) {
	if c == nil || c.dlg == nil {
		return
	}
	dialogState(c.dlg).set(name, value)
}

// Header returns a value that arrived on this call. The name match is
// case-insensitive. The latest message that carried the name wins.
func (c *Call) Header(name string) (string, bool) {
	if c == nil || c.dlg == nil {
		return "", false
	}
	return dialogState(c.dlg).get(name)
}

// SetHeader stores a caller header for requests this endpoint sends outside
// a dialog, including REGISTER. An owned hop-by-hop name is refused.
func (e *Endpoint) SetHeader(name, value string) {
	if e == nil {
		return
	}
	endpointState(e).set(name, value)
}

// Header returns a value that arrived outside a dialog, including a REGISTER
// response. The name match is case-insensitive.
func (e *Endpoint) Header(name string) (string, bool) {
	if e == nil {
		return "", false
	}
	return endpointState(e).get(name)
}

func (d *dialog) storeHeaders(hs []Header) {
	if d == nil {
		return
	}
	st := dialogState(d)
	for _, h := range hs {
		st.set(h.Name, h.Value)
	}
}

// pushHeaders installs hs for the next stamp, then restores the previous set.
func (d *dialog) pushHeaders(hs []Header) func() {
	st := dialogState(d)
	st.mu.Lock()
	prev := append([]Header(nil), st.out...)
	st.mu.Unlock()
	for _, h := range hs {
		st.set(h.Name, h.Value)
	}
	return func() {
		st.mu.Lock()
		st.out = prev
		st.mu.Unlock()
	}
}

// stamp writes stored caller headers onto an outbound message. extra wins
// over the dialog set, which wins over the endpoint set. Owned hop-by-hop
// names are left as the stack wrote them.
func (d *dialog) stamp(msg sip.Message, extra []Header) {
	if d == nil || msg == nil {
		return
	}
	lists := [][]Header{extra, dialogState(d).outbound()}
	if d.ep != nil {
		lists = append(lists, endpointState(d.ep).outbound())
	}
	applyCustom(msg, lists...)
}

func (e *Endpoint) stamp(msg sip.Message) {
	if e == nil || msg == nil {
		return
	}
	applyCustom(msg, endpointState(e).outbound())
}

func applyCustom(msg sip.Message, lists ...[]Header) {
	if msg == nil {
		return
	}
	seen := map[string]struct{}{}
	for _, list := range lists {
		for _, h := range list {
			name := strings.TrimSpace(h.Name)
			key := strings.ToLower(name)
			if key == "" || ownedHeader(name) {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if headerHasValue(msg, name, h.Value) {
				continue
			}
			msg.AppendHeader(sip.NewHeader(name, h.Value))
		}
	}
}

func headerHasValue(msg sip.Message, name, value string) bool {
	for _, h := range msg.GetHeaders(name) {
		if h != nil && h.Value() == value {
			return true
		}
	}
	return false
}

func messageHeaders(msg sip.Message) []Header {
	if msg == nil {
		return nil
	}
	var hs []sip.Header
	switch m := msg.(type) {
	case *sip.Request:
		hs = m.Headers()
	case *sip.Response:
		hs = m.Headers()
	default:
		return nil
	}
	out := make([]Header, 0, len(hs))
	for _, h := range hs {
		if h == nil {
			continue
		}
		out = append(out, Header{Name: h.Name(), Value: h.Value()})
	}
	return out
}

func skipArrived(msg sip.Message) bool {
	req, ok := msg.(*sip.Request)
	if !ok || req == nil || req.CSeq() == nil {
		return false
	}
	switch req.CSeq().MethodName {
	case sip.ACK, sip.CANCEL:
		return true
	default:
		return false
	}
}

// noteArrived copies the headers on msg onto this dialog before the handler
// acts on it. ACK and CANCEL do not replace the INVITE the app accepts.
func (d *dialog) noteArrived(msg sip.Message) {
	if d == nil || msg == nil || skipArrived(msg) {
		return
	}
	dialogState(d).note(messageHeaders(msg))
}

func (e *Endpoint) noteArrived(msg sip.Message) {
	if e == nil || msg == nil {
		return
	}
	endpointState(e).note(messageHeaders(msg))
}

// customFields is the caller headers as sipgo headers, for a response
// built inside sipgo. Owned hop-by-hop names are omitted. Dialog headers
// win over the endpoint set.
func (d *dialog) customFields() []sip.Header {
	if d == nil {
		return nil
	}
	lists := [][]Header{dialogState(d).outbound()}
	if d.ep != nil {
		lists = append(lists, endpointState(d.ep).outbound())
	}
	return fieldsFrom(lists...)
}

func (e *Endpoint) customFields() []sip.Header {
	if e == nil {
		return nil
	}
	return fieldsFrom(endpointState(e).outbound())
}

func fieldsFrom(lists ...[]Header) []sip.Header {
	seen := map[string]struct{}{}
	var out []sip.Header
	for _, list := range lists {
		for _, h := range list {
			name := strings.TrimSpace(h.Name)
			key := strings.ToLower(name)
			if key == "" || ownedHeader(name) {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, sip.NewHeader(name, h.Value))
		}
	}
	return out
}
