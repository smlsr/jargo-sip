package sip

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

// sessionSeconds is the Session-Expires delta. Zero means timers are off.
func sessionSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	secs := int(d / time.Second)
	if secs < 1 {
		return 1
	}
	return secs
}

func (d *dialog) adoptOutboundSession(res *sip.Response) {
	if d == nil || d.ep == nil || d.ep.cfg.SessionTimer <= 0 {
		return
	}
	secs, role, ok := headerDelta(res, "Session-Expires")
	if !ok {
		secs = d.timerOffer
		role = "uac"
	}
	if secs <= 0 {
		return
	}
	if role == "" {
		role = "uac"
	}
	d.mu.Lock()
	d.sessionInterval = time.Duration(secs) * time.Second
	d.sessionWeRefresh = role != "uas"
	d.mu.Unlock()
}

// noteInboundSession records a peer timer and returns the Session-Expires
// we echo on the 200. A zero SessionTimer leaves the header off.
func (e *Endpoint) noteInboundSession(d *dialog, req *sip.Request) sip.Header {
	if e == nil || e.cfg.SessionTimer <= 0 || req == nil {
		return nil
	}
	secs, role, ok := headerDelta(req, "Session-Expires")
	if !ok {
		return nil
	}
	we := role == "uas"
	if role != "uas" {
		role = "uac"
	}
	d.mu.Lock()
	d.sessionInterval = time.Duration(secs) * time.Second
	d.sessionWeRefresh = we
	d.mu.Unlock()
	return sip.NewHeader("Session-Expires", fmt.Sprintf("%d;refresher=%s", secs, role))
}

func (d *dialog) armStoredSession() {
	if d == nil {
		return
	}
	d.mu.Lock()
	interval := d.sessionInterval
	we := d.sessionWeRefresh
	d.mu.Unlock()
	d.armSession(interval, we)
}

func (d *dialog) rearmSession() {
	if d == nil {
		return
	}
	d.mu.Lock()
	interval := d.sessionInterval
	we := d.sessionWeRefresh
	ended := d.ended
	d.mu.Unlock()
	if ended || interval <= 0 {
		return
	}
	d.armSession(interval, we)
}

func (d *dialog) armSession(interval time.Duration, we bool) {
	if d == nil || interval <= 0 {
		return
	}
	d.mu.Lock()
	if d.ended {
		d.mu.Unlock()
		return
	}
	d.sessionGen++
	gen := d.sessionGen
	d.sessionInterval = interval
	d.sessionWeRefresh = we
	d.stopSessionLocked()
	d.mu.Unlock()

	half := interval / 2
	if half <= 0 {
		half = interval
	}
	var refresh *time.Timer
	if we {
		refresh = time.AfterFunc(half, func() { d.onSessionRefresh(gen) })
	}
	expiry := time.AfterFunc(interval, func() { d.onSessionExpiry(gen) })
	d.mu.Lock()
	if d.sessionGen != gen || d.ended {
		if refresh != nil {
			refresh.Stop()
		}
		expiry.Stop()
		d.mu.Unlock()
		return
	}
	d.sessionRefresh = refresh
	d.sessionExpiry = expiry
	d.mu.Unlock()
}

func (d *dialog) stopSession() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.sessionGen++
	d.stopSessionLocked()
	d.mu.Unlock()
}

func (d *dialog) stopSessionLocked() {
	if d.sessionRefresh != nil {
		d.sessionRefresh.Stop()
		d.sessionRefresh = nil
	}
	if d.sessionExpiry != nil {
		d.sessionExpiry.Stop()
		d.sessionExpiry = nil
	}
}

func (d *dialog) onSessionRefresh(gen uint64) {
	d.mu.Lock()
	if d.ended || d.sessionGen != gen {
		d.mu.Unlock()
		return
	}
	call := d.call
	d.mu.Unlock()
	// No short deadline. The Call-ID owner waits 2.1–4s after a 491, and that
	// wait has to finish so the one retry can be sent. Expiry still hangs up.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if call != nil {
		go func() {
			select {
			case <-call.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	err := d.timerRefresh(ctx)
	d.mu.Lock()
	stale := d.ended || d.sessionGen != gen
	d.mu.Unlock()
	if stale {
		return
	}
	if err != nil {
		slog.Error("session refresh failed", "err", err)
		d.endSession()
		return
	}
	d.rearmSession()
}

func (d *dialog) onSessionExpiry(gen uint64) {
	d.mu.Lock()
	if d.ended || d.sessionGen != gen {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	slog.Info("session timer expired")
	d.endSession()
}

func (d *dialog) endSession() {
	d.mu.Lock()
	call := d.call
	d.mu.Unlock()
	if call != nil {
		_ = call.Hangup(context.Background())
	}
}

func (d *dialog) timerRefresh(ctx context.Context) error {
	res, err := d.sessionAttempt(ctx)
	if err != nil {
		return err
	}
	if res.StatusCode == sip.StatusRequestPending {
		// The glare wait is not clipped by a refresh deadline. A 491 is one
		// retry, not a hangup. A dead call cancels ctx from onSessionRefresh.
		if err := waitGlare(ctx, d.ownsCallID); err != nil {
			return err
		}
		res, err = d.sessionAttempt(ctx)
		if err != nil {
			return err
		}
	}
	if res.StatusCode != sip.StatusOK {
		return fmt.Errorf("session refresh failed: %d", res.StatusCode)
	}
	return nil
}

func (d *dialog) sessionAttempt(ctx context.Context) (*sip.Response, error) {
	d.mu.Lock()
	method := sip.INVITE
	if d.allowUpdate {
		method = sip.UPDATE
	}
	interval := d.sessionInterval
	role := "uas"
	if d.uac {
		role = "uac"
	}
	d.mu.Unlock()
	secs := int(interval / time.Second)
	if secs < 1 {
		secs = 1
	}
	extra := []sip.Header{sip.NewHeader("Session-Expires", fmt.Sprintf("%d;refresher=%s", secs, role))}
	phase := offerOutInvite
	if method == sip.UPDATE {
		phase = offerOutUpdate
	}
	if err := d.waitAndReserve(ctx, method, method == sip.INVITE); err != nil {
		return nil, err
	}
	defer d.endOutbound(phase)

	var body []byte
	var err error
	if method == sip.INVITE {
		body, err = d.currentOfferSDP()
		if err != nil {
			return nil, err
		}
	}
	var req *sip.Request
	if method == sip.UPDATE {
		req, err = d.craft(method, nil, "", extra)
	} else {
		req, err = d.craft(method, body, "application/sdp", extra)
	}
	if err != nil {
		return nil, err
	}
	if d.ep.cfg.SessionTimer > 0 {
		req.RemoveHeader("Supported")
		req.AppendHeader(timerSupported())
	}
	res, err := d.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == sip.StatusOK && method == sip.INVITE {
		if err := d.ackInvite(req, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// currentOfferSDP repeats the SDP we last sent, with o= one higher.
// c=, m=, and direction stay, so a refresh does not move the latch.
func (d *dialog) currentOfferSDP() ([]byte, error) {
	raw := d.currentSDP()
	desc, err := parseSDPText(string(raw))
	if err != nil {
		return nil, err
	}
	next := d.origin.version + 1
	if next <= desc.Version {
		next = desc.Version + 1
	}
	d.origin.version = next
	desc.Version = next
	return desc.Marshal()
}

func headerDelta(msg sip.Message, name string) (int, string, bool) {
	raw := firstHeader(msg, name)
	if raw == "" {
		return 0, "", false
	}
	return parseDeltaSeconds(raw)
}

func parseMinSE(msg sip.Message) int {
	raw := firstHeader(msg, "Min-SE")
	if raw == "" {
		return 0
	}
	n, _, ok := parseDeltaSeconds(raw)
	if !ok {
		return 0
	}
	return n
}

func firstHeader(msg sip.Message, name string) string {
	if msg == nil {
		return ""
	}
	hs := msg.GetHeaders(name)
	if len(hs) == 0 || hs[0] == nil {
		return ""
	}
	return hs[0].Value()
}

func parseDeltaSeconds(raw string) (int, string, bool) {
	parts := strings.Split(raw, ";")
	n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || n <= 0 {
		return 0, "", false
	}
	role := ""
	for _, part := range parts[1:] {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "refresher") {
			role = strings.ToLower(strings.TrimSpace(val))
		}
	}
	return n, role, true
}
