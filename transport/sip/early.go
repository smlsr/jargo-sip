package sip

import (
	"bytes"
	"context"
	"errors"
)

// ApplyEarly handles a 183. A body with no SDP leaves current unchanged and
// plays nothing. A body with SDP starts the receiver, the latch, the jitter
// buffer, and the sender toward that address. The sender may transmit codec
// silence so the peer can latch. Frames that arrive here stay queued for
// ReadPCM. This package does not generate a 183. The basic UAS path is
// 100 Trying and then 200 OK.
func ApplyEarly(ctx context.Context, pair *RTPPair, current *Media, offered Description, body []byte) (*Media, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return current, nil
	}
	return noteAnswer(ctx, pair, current, offered, body)
}

// ConfirmAnswer handles a 200 with SDP. A nil session starts media.
// The same connection and port keep the sender, the latch, and every frame
// already waiting on ReadPCM. A different address returns the latch to
// SDPTarget. The local SSRC and sequence stay.
func ConfirmAnswer(ctx context.Context, pair *RTPPair, current *Media, offered Description, body []byte) (*Media, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return current, errors.New("sdp answer is missing")
	}
	return noteAnswer(ctx, pair, current, offered, body)
}

func noteAnswer(ctx context.Context, pair *RTPPair, current *Media, offered Description, body []byte) (*Media, error) {
	if ctx == nil {
		return current, errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return current, err
	}
	remote, dir, err := remoteFromAnswer(offered, body)
	if err != nil {
		return current, err
	}
	if current == nil {
		m, serr := StartMedia(ctx, pair, remote)
		if serr != nil {
			return nil, serr
		}
		if dir != DirectionSendRecv {
			if derr := m.SetDirection(dir); derr != nil {
				_ = m.Close()
				return nil, derr
			}
		}
		return m, nil
	}
	if err := current.Retarget(remote); err != nil {
		return current, err
	}
	if err := current.SetDirection(dir); err != nil {
		return current, err
	}
	return current, nil
}

func remoteFromAnswer(offered Description, body []byte) (RemoteMedia, Direction, error) {
	desc, err := Parse(string(body))
	if err != nil {
		return RemoteMedia{}, "", err
	}
	n, err := AcceptAnswer(offered, desc)
	if err != nil {
		return RemoteMedia{}, "", err
	}
	remote := RemoteMedia{
		Connection:   cloneIP(n.Connection),
		RTPPort:      n.RTPPort,
		RTCPPort:     n.RTCPPort,
		RTCPExplicit: n.RTCPExplicit,
		RTCPIP:       cloneIP(n.RTCPIP),
		Audio:        n.Audio,
	}
	if n.DTMF != nil {
		te := *n.DTMF
		remote.DTMF = &te
	}
	return remote, n.Direction, nil
}
