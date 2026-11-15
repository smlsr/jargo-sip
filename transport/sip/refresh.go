package sip

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/emiago/sipgo/sip"
)

func (d *dialog) sendRefresh(ctx context.Context, method sip.RequestMethod, dir Direction) error {
	res, err := d.attempt(ctx, method, dir)
	if err != nil {
		return err
	}
	if res.StatusCode != sip.StatusRequestPending {
		if res.StatusCode != sip.StatusOK {
			return fmt.Errorf("refresh failed: %d", res.StatusCode)
		}
		return nil
	}
	if err := waitGlare(ctx, d.ownsCallID); err != nil {
		return err
	}
	res, err = d.attempt(ctx, method, dir)
	if err != nil {
		return err
	}
	if res.StatusCode != sip.StatusOK {
		return fmt.Errorf("refresh failed: %d", res.StatusCode)
	}
	return nil
}

func (d *dialog) attempt(ctx context.Context, method sip.RequestMethod, dir Direction) (*sip.Response, error) {
	phase := offerOutInvite
	if method == sip.UPDATE {
		phase = offerOutUpdate
	}
	if err := d.waitAndReserve(ctx, method, true); err != nil {
		return nil, err
	}
	defer d.endOutbound(phase)

	desc, raw, err := d.makeOffer(dir)
	if err != nil {
		return nil, err
	}
	req, err := d.newRequest(method, raw)
	if err != nil {
		return nil, err
	}
	res, err := d.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != sip.StatusOK {
		return res, nil
	}
	if err := d.applyAnswer(res.Body(), desc, dir); err != nil {
		return nil, err
	}
	d.storeContact(res)
	d.storeSDP(raw)
	if method == sip.INVITE {
		if err := d.ackInvite(req, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (d *dialog) makeOffer(dir Direction) (Description, []byte, error) {
	d.mu.Lock()
	pair := d.pair
	d.mu.Unlock()
	if pair == nil {
		return Description{}, nil, errors.New("rtp pair is missing")
	}
	return d.origin.Offer(d.ep.mediaAddr(pair), d.ep.cfg, dir)
}

func (d *dialog) applyAnswer(raw []byte, offered Description, dir Direction) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("answer sdp is missing")
	}
	answer, err := parseSDPText(string(raw))
	if err != nil {
		return err
	}
	neg, err := AcceptAnswer(offered, answer)
	if err != nil {
		return err
	}
	var dtmf *Format
	if neg.DTMF != nil {
		te := *neg.DTMF
		dtmf = &te
	}
	remote := RemoteMedia{
		Connection:   cloneIP(neg.Connection),
		RTPPort:      neg.RTPPort,
		RTCPPort:     neg.RTCPPort,
		RTCPExplicit: neg.RTCPExplicit,
		RTCPIP:       cloneIP(neg.RTCPIP),
		Audio:        neg.Audio,
		DTMF:         dtmf,
	}
	return d.commitMedia(remote, dir)
}
