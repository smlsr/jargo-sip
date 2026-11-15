package sip

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ErrEndpointClosed is returned by Accept after Close.
var ErrEndpointClosed = errors.New("endpoint is closed")

var errBadCSeq = errors.New("cseq is out of order")

// Party is the peer as currently stored on a dialog.
type Party struct {
	URI          string
	DisplayName  string
	CallID       string
	LocalTag     string
	RemoteTag    string
	RemoteTarget string
}

// Call is one established dialog and its RTP session.
type Call struct {
	ep   *Endpoint
	dlg  *dialog
	done chan struct{}
	once sync.Once
	max  *time.Timer
}

// Done closes when the dialog ends.
func (c *Call) Done() <-chan struct{} {
	if c == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return c.done
}

// Remote returns a copy of the peer identity.
func (c *Call) Remote() Party {
	if c == nil || c.dlg == nil {
		return Party{}
	}
	c.dlg.mu.Lock()
	defer c.dlg.mu.Unlock()
	return c.dlg.party
}

// ReadPCM returns one 20 ms frame. Stopped playout and underrun return concealment.
func (c *Call) ReadPCM() ([]byte, error) {
	if c == nil || c.dlg == nil || c.dlg.media == nil {
		return nil, ErrMediaClosed
	}
	select {
	case <-c.done:
		return nil, ErrMediaClosed
	default:
	}
	return c.dlg.media.ReadPCM()
}

// WritePCM queues 8000 Hz PCM for the sender.
func (c *Call) WritePCM(pcm []byte) error {
	if c == nil || c.dlg == nil || c.dlg.media == nil {
		return ErrMediaClosed
	}
	select {
	case <-c.done:
		return ErrMediaClosed
	default:
	}
	return c.dlg.media.WritePCM(pcm)
}

// Hangup sends BYE on an established dialog.
func (c *Call) Hangup(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("nil context")
	}
	select {
	case <-c.done:
		return nil
	default:
	}
	err := c.dlg.sendBye(ctx)
	c.finish()
	return err
}

func (c *Call) finish() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		if c.max != nil {
			c.max.Stop()
		}
		if c.dlg != nil {
			c.dlg.stopSession()
			c.dlg.stopInvite()
			c.dlg.markEnded()
			if c.dlg.pair != nil {
				_ = c.dlg.pair.Close()
			}
			if c.ep != nil && c.ep.dialogs != nil {
				c.ep.dialogs.drop(c.dlg.id)
			}
		}
		close(c.done)
	})
}

// Hold offers sendonly. UPDATE is used when the peer allowed it.
func (c *Call) Hold(ctx context.Context) error {
	return c.refresh(ctx, DirectionSendOnly)
}

// Resume offers sendrecv. UPDATE is used when the peer allowed it.
func (c *Call) Resume(ctx context.Context) error {
	return c.refresh(ctx, DirectionSendRecv)
}

func (c *Call) refresh(ctx context.Context, dir Direction) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if c == nil || c.dlg == nil {
		return ErrMediaClosed
	}
	select {
	case <-c.done:
		return ErrMediaClosed
	default:
	}
	c.dlg.mu.Lock()
	method := sip.INVITE
	if c.dlg.allowUpdate {
		method = sip.UPDATE
	}
	c.dlg.mu.Unlock()
	return c.dlg.sendRefresh(ctx, method, dir)
}

type offerPhase int

const (
	offerIdle offerPhase = iota
	offerInInvite
	offerOutInvite
	offerInUpdate
	offerOutUpdate
)

type dialog struct {
	ep         *Endpoint
	id         string
	sess       *sipgo.DialogServerSession
	ownsCallID bool

	mu           sync.Mutex
	wake         chan struct{}
	localCSeq    uint32
	localInvite  uint32
	remoteCSeq   uint32
	remoteInvite uint32
	haveRemote   bool
	seenRemote   map[uint32]struct{}

	offer      offerPhase
	offerGen   uint64
	inOffer    bool
	outOffer   bool
	ackPending bool
	ended      bool
	byeSent    bool

	party       Party
	remoteURI   sip.Uri
	haveURI     bool
	allowUpdate bool
	localFrom   sip.Header
	remoteTo    sip.Header
	callID      sip.Header

	origin          Origin
	localSDP        []byte
	remote          RemoteMedia
	haveRemoteMedia bool
	direction       Direction

	pair  *RTPPair
	media *Media
	call  *Call

	tx         sip.ServerTransaction
	reliableOK bool
	rseq       uint32
	pracked    uint32
	provCancel context.CancelFunc
	extraAck   chan *sip.Request

	// Outbound INVITE and REFER.
	uac          bool
	finalSent    bool
	holdFinish   bool
	referByeSeq  uint32
	referByeBase uint32
	referBusy    bool
	invite       *sip.Request
	inviteTx     sip.ClientTransaction
	client       *sipgo.DialogClientSession
	prackSeen    map[uint32]struct{}
	notes        chan referNote

	timerOffer       int
	sessionInterval  time.Duration
	sessionWeRefresh bool
	sessionGen       uint64
	sessionRefresh   *time.Timer
	sessionExpiry    *time.Timer
}

func newDialog(ep *Endpoint, sess *sipgo.DialogServerSession) (*dialog, error) {
	origin, err := NewOrigin()
	if err != nil {
		return nil, err
	}
	return &dialog{
		ep:       ep,
		id:       sess.ID,
		sess:     sess,
		origin:   origin,
		wake:     make(chan struct{}),
		extraAck: make(chan *sip.Request, 1),
	}, nil
}

func (d *dialog) wakeLocked() {
	close(d.wake)
	d.wake = make(chan struct{})
}

func (d *dialog) markEnded() {
	d.mu.Lock()
	d.ended = true
	d.offer = offerIdle
	d.inOffer = false
	d.outOffer = false
	d.ackPending = false
	d.wakeLocked()
	d.mu.Unlock()
	d.stopProvisional()
}

func (d *dialog) noteInitial(req *sip.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cseq := req.CSeq(); cseq != nil {
		d.remoteCSeq = cseq.SeqNo
		d.remoteInvite = cseq.SeqNo
		d.haveRemote = true
	}
	if cid := req.CallID(); cid != nil {
		d.party.CallID = cid.Value()
	}
	if from := req.From(); from != nil {
		d.party.URI = from.Address.String()
		d.party.DisplayName = from.DisplayName
		if tag, ok := from.Params.Get("tag"); ok {
			d.party.RemoteTag = tag
		}
	}
	if d.sess != nil && d.sess.InviteRequest != nil && d.sess.InviteRequest.To() != nil {
		if tag, ok := d.sess.InviteRequest.To().Params.Get("tag"); ok {
			d.party.LocalTag = tag
		}
	}
	if c := req.Contact(); c != nil {
		d.remoteURI = *c.Address.Clone()
		d.haveURI = true
		d.party.RemoteTarget = c.Address.String()
	}
	d.allowUpdate = hasToken(req, "Allow", "UPDATE")
	d.reliableOK = wants100rel(req)
}

// noteRemote updates the remote sequence. ACK and CANCEL must copy the
// INVITE CSeq and do not advance it.
func (d *dialog) noteRemote(req *sip.Request) error {
	cseq := req.CSeq()
	if cseq == nil {
		return errBadCSeq
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	seq := cseq.SeqNo
	switch cseq.MethodName {
	case sip.ACK, sip.CANCEL:
		if seq != d.remoteInvite {
			return errBadCSeq
		}
		return nil
	default:
		if _, seen := d.seenRemote[seq]; seen {
			return errBadCSeq
		}
		if d.haveRemote && seq <= d.remoteCSeq {
			return errBadCSeq
		}
		if d.seenRemote == nil {
			d.seenRemote = map[uint32]struct{}{}
		}
		d.seenRemote[seq] = struct{}{}
		d.haveRemote = true
		d.remoteCSeq = seq
		if cseq.MethodName == sip.INVITE {
			d.remoteInvite = seq
		}
		return nil
	}
}

func (d *dialog) nextCSeq(method sip.RequestMethod) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch method {
	case sip.ACK, sip.CANCEL:
		if d.localInvite == 0 {
			return 0, errors.New("invite cseq is missing")
		}
		return d.localInvite, nil
	default:
		d.localCSeq++
		if method == sip.INVITE {
			d.localInvite = d.localCSeq
		}
		return d.localCSeq, nil
	}
}

func (d *dialog) glareLocked(method sip.RequestMethod, hasSDP bool) (code int, retry int, blocked bool) {
	switch method {
	case sip.INVITE:
		if d.offer == offerOutInvite || d.offer == offerOutUpdate {
			return sip.StatusRequestPending, 0, true
		}
		if d.offer == offerInInvite || d.offer == offerInUpdate {
			return sip.StatusInternalServerError, randRetryAfter(), true
		}
	case sip.UPDATE:
		if d.offer == offerInUpdate {
			return sip.StatusInternalServerError, randRetryAfter(), true
		}
		if !hasSDP {
			return 0, 0, false
		}
		if d.offer == offerOutInvite || d.offer == offerOutUpdate {
			return sip.StatusRequestPending, 0, true
		}
		if d.offer == offerInInvite {
			return sip.StatusInternalServerError, randRetryAfter(), true
		}
	default:
		return 0, 0, false
	}
	return 0, 0, false
}

func (d *dialog) beginInbound(method sip.RequestMethod, hasSDP bool) (int, int, bool, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ended {
		return sip.StatusCallTransactionDoesNotExists, 0, false, 0
	}
	if code, retry, blocked := d.glareLocked(method, hasSDP); blocked {
		return code, retry, false, 0
	}
	if method == sip.UPDATE && !hasSDP {
		return 0, 0, false, 0
	}
	switch method {
	case sip.INVITE:
		d.offer = offerInInvite
	case sip.UPDATE:
		d.offer = offerInUpdate
	default:
		return sip.StatusInternalServerError, 0, false, 0
	}
	d.offerGen++
	d.inOffer = hasSDP
	return 0, 0, true, d.offerGen
}

func (d *dialog) endInbound(phase offerPhase, gen uint64) {
	d.mu.Lock()
	if d.offer == phase && d.offerGen == gen {
		d.offer = offerIdle
		d.inOffer = false
	}
	d.wakeLocked()
	d.mu.Unlock()
}

func (d *dialog) waitAndReserve(ctx context.Context, method sip.RequestMethod, withSDP bool) error {
	var phase offerPhase
	switch method {
	case sip.UPDATE:
		phase = offerOutUpdate
	case sip.INVITE:
		phase = offerOutInvite
	default:
		return fmt.Errorf("method %s is not a refresh", method)
	}
	for {
		d.mu.Lock()
		if d.ended {
			d.mu.Unlock()
			return errors.New("dialog ended")
		}
		if d.offer == offerIdle && !d.ackPending {
			d.offer = phase
			d.outOffer = withSDP
			d.mu.Unlock()
			return nil
		}
		wake := d.wake
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
}

func (d *dialog) endOutbound(phase offerPhase) {
	d.mu.Lock()
	if d.offer == phase {
		d.offer = offerIdle
		d.outOffer = false
	}
	d.wakeLocked()
	d.mu.Unlock()
}

func (d *dialog) storeContact(msg sip.Message) {
	c := contactOf(msg)
	if c == nil {
		return
	}
	d.mu.Lock()
	d.remoteURI = *c.Address.Clone()
	d.haveURI = true
	d.party.RemoteTarget = c.Address.String()
	if hasToken(msg, "Allow", "UPDATE") {
		d.allowUpdate = true
	}
	d.mu.Unlock()
}

func (d *dialog) capture(res *sip.Response) {
	if res == nil || res.To() == nil || res.From() == nil {
		return
	}
	from := res.To().AsFrom()
	to := res.From().AsTo()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.localFrom = sip.HeaderClone(&from)
	d.remoteTo = sip.HeaderClone(&to)
	if cid := res.CallID(); cid != nil {
		d.callID = sip.HeaderClone(cid)
	}
	if tag, ok := res.To().Params.Get("tag"); ok {
		d.party.LocalTag = tag
	}
	if tag, ok := res.From().Params.Get("tag"); ok {
		d.party.RemoteTag = tag
	}
}

func (d *dialog) storeSDP(raw []byte) {
	d.mu.Lock()
	d.localSDP = append([]byte(nil), raw...)
	d.mu.Unlock()
}

func (d *dialog) currentSDP() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.localSDP...)
}

func (d *dialog) setAckPending(on bool) {
	d.mu.Lock()
	d.ackPending = on
	d.wakeLocked()
	d.mu.Unlock()
}

func (d *dialog) commitMedia(remote RemoteMedia, dir Direction) error {
	d.mu.Lock()
	m := d.media
	d.mu.Unlock()
	if m == nil {
		return errors.New("media is missing")
	}
	if err := m.Retarget(remote); err != nil {
		return err
	}
	if err := applyMediaDirection(m, dir); err != nil {
		return err
	}
	d.mu.Lock()
	d.remote = remote
	d.haveRemoteMedia = true
	d.direction = dir
	d.mu.Unlock()
	return nil
}

// commitIfSent commits a re-INVITE or UPDATE only after send returns nil.
// A failed send leaves the previous remote, direction, and SDP in place.
func (d *dialog) commitIfSent(remote RemoteMedia, dir Direction, sdp []byte, send func() error) error {
	if err := send(); err != nil {
		return err
	}
	if err := d.commitMedia(remote, dir); err != nil {
		return err
	}
	d.storeSDP(sdp)
	return nil
}

func applyMediaDirection(m *Media, dir Direction) error {
	if m == nil {
		return errors.New("media is missing")
	}
	norm, err := normalizeDirection(dir)
	if err != nil {
		return err
	}
	if err := m.SetDirection(norm); err != nil {
		return err
	}
	on := norm == DirectionSendRecv || norm == DirectionRecvOnly
	return m.SetPlayout(on)
}

func mediaFromOffer(offer Description, cfg Config) (RemoteMedia, Direction, error) {
	audio, err := chooseAudio(offer, cfg)
	if err != nil {
		return RemoteMedia{}, "", err
	}
	var dtmf *Format
	if te, derr := offer.DTMF(); derr == nil {
		dtmf = &te
	}
	dir := offer.Direction
	if dir == "" {
		dir = DirectionSendRecv
	}
	return RemoteMedia{
		Connection:   cloneIP(offer.Connection),
		RTPPort:      offer.RTPPort,
		RTCPPort:     offer.RTCPPort,
		RTCPExplicit: offer.RTCPExplicit,
		RTCPIP:       cloneIP(offer.RTCPIP),
		Audio:        audio,
		DTMF:         dtmf,
	}, dir, nil
}

func (d *dialog) newRequest(method sip.RequestMethod, body []byte) (*sip.Request, error) {
	return d.craft(method, body, "application/sdp", nil)
}

func (d *dialog) craft(method sip.RequestMethod, body []byte, ctype string, extra []sip.Header) (*sip.Request, error) {
	d.mu.Lock()
	if d.ended && method != sip.BYE {
		d.mu.Unlock()
		return nil, errors.New("dialog ended")
	}
	if !d.haveURI {
		d.mu.Unlock()
		return nil, errors.New("remote target is missing")
	}
	uri := d.remoteURI
	from := d.localFrom
	to := d.remoteTo
	callID := d.callID
	d.mu.Unlock()
	if from == nil || to == nil || callID == nil {
		return nil, errors.New("dialog headers are missing")
	}
	seq, err := d.nextCSeq(method)
	if err != nil {
		return nil, err
	}
	req := sip.NewRequest(method, uri)
	req.AppendHeader(d.ep.via())
	req.AppendHeader(sip.HeaderClone(from))
	req.AppendHeader(sip.HeaderClone(to))
	req.AppendHeader(sip.HeaderClone(callID))
	req.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: method})
	max := sip.MaxForwardsHeader(70)
	req.AppendHeader(&max)
	req.AppendHeader(sip.HeaderClone(&d.ep.contact))
	req.AppendHeader(d.ep.allowHeader())
	req.AppendHeader(d.ep.supportedHeader())
	for _, h := range extra {
		if h != nil {
			req.AppendHeader(h)
		}
	}
	if len(body) > 0 {
		if ctype != "" {
			ct := sip.ContentTypeHeader(ctype)
			req.AppendHeader(&ct)
		}
		req.SetBody(body)
	} else {
		req.SetBody(nil)
	}
	req.SetTransport("UDP")
	dest := uri.HostPort()
	if uri.Port == 0 {
		dest = net.JoinHostPort(uri.Host, "5060")
	}
	req.SetDestination(dest)
	req.Laddr = d.ep.listen
	return req, nil
}

func (d *dialog) roundTrip(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	tx, err := d.ep.beginTx(ctx, req)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()
	for {
		select {
		case res := <-tx.Responses():
			if res == nil {
				continue
			}
			if res.IsProvisional() {
				continue
			}
			return res, nil
		case <-tx.Done():
			if err := tx.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("transaction ended")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (d *dialog) ackInvite(invite *sip.Request, res *sip.Response) error {
	if invite == nil || res == nil || res.Contact() == nil {
		return errors.New("ack is missing the dialog")
	}
	uri := res.Contact().Address
	if uri.Host == "" {
		uri = invite.Recipient
	}
	ack := sip.NewRequest(sip.ACK, uri)
	if via := invite.Via(); via != nil {
		ack.AppendHeader(via.Clone())
	}
	if from := invite.From(); from != nil {
		ack.AppendHeader(sip.HeaderClone(from))
	}
	if to := res.To(); to != nil {
		ack.AppendHeader(sip.HeaderClone(to))
	}
	if cid := invite.CallID(); cid != nil {
		ack.AppendHeader(sip.HeaderClone(cid))
	}
	seq := uint32(0)
	if cseq := invite.CSeq(); cseq != nil {
		seq = cseq.SeqNo
	}
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: sip.ACK})
	max := sip.MaxForwardsHeader(70)
	ack.AppendHeader(&max)
	ack.SetBody(nil)
	ack.SetTransport("UDP")
	dest := uri.HostPort()
	if uri.Port == 0 {
		dest = net.JoinHostPort(uri.Host, "5060")
	}
	ack.SetDestination(dest)
	ack.Laddr = d.ep.listen
	return d.ep.writeReq(ack)
}

func (d *dialog) sendBye(ctx context.Context) error {
	d.mu.Lock()
	if d.byeSent || d.localFrom == nil {
		d.mu.Unlock()
		return nil
	}
	d.byeSent = true
	d.mu.Unlock()
	req, err := d.newRequest(sip.BYE, nil)
	if err != nil {
		return err
	}
	res, err := d.roundTrip(ctx, req)
	if err != nil {
		return err
	}
	if res.StatusCode != sip.StatusOK {
		return fmt.Errorf("bye failed: %d", res.StatusCode)
	}
	return nil
}

func randN(n int) int {
	if n <= 1 {
		return 0
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0
	}
	return int(binary.BigEndian.Uint32(buf[:]) % uint32(n))
}

func randRetryAfter() int {
	return randN(11)
}

func glareDelay(ownsCallID bool) time.Duration {
	if ownsCallID {
		return time.Duration(2100+randN(1901)) * time.Millisecond
	}
	return time.Duration(randN(2001)) * time.Millisecond
}

func waitGlare(ctx context.Context, ownsCallID bool) error {
	timer := time.NewTimer(glareDelay(ownsCallID))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type dialogTable struct {
	mu   sync.Mutex
	byID map[string]*dialog
	auth map[string]*authState
}

func newDialogTable() *dialogTable {
	return &dialogTable{
		byID: map[string]*dialog{},
		auth: map[string]*authState{},
	}
}

func (t *dialogTable) put(d *dialog) {
	if t == nil || d == nil {
		return
	}
	t.mu.Lock()
	t.byID[d.id] = d
	t.mu.Unlock()
}

func (t *dialogTable) get(id string) *dialog {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byID[id]
}

func (t *dialogTable) drop(id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.byID, id)
	t.mu.Unlock()
}

func (t *dialogTable) all() []*dialog {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*dialog, 0, len(t.byID))
	for _, d := range t.byID {
		out = append(out, d)
	}
	return out
}

// find matches Replaces as the UA that receives the new INVITE sees the dialog.
// toTag is our local tag. fromTag is the peer's tag.
func (t *dialogTable) find(callID, toTag, fromTag string) *dialog {
	for _, d := range t.all() {
		d.mu.Lock()
		match := d.party.CallID == callID && d.party.LocalTag == toTag && d.party.RemoteTag == fromTag
		d.mu.Unlock()
		if match {
			return d
		}
	}
	return nil
}

func (t *dialogTable) calls() []*Call {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*Call, 0, len(t.byID))
	for _, d := range t.byID {
		if d.call != nil {
			out = append(out, d.call)
		}
	}
	return out
}
