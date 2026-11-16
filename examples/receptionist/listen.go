package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/smlsr/jargo-sip/transport/sip"
)

// listenConfig is the SIP listener. Audio on the trunk stays 8 kHz G.711.
// The pipeline rate is separate and is resampled at the transport edge.
type listenConfig struct {
	ListenIP    string
	Port        int
	Domain      string
	User        string
	DisplayName string
	AllowFrom   []string
	AdvertiseIP string
}

func listenConfigFromEnv() (listenConfig, error) {
	port := 5060
	if raw := strings.TrimSpace(os.Getenv("SIP_PORT")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return listenConfig{}, err
		}
		port = n
	}
	allow := splitList(os.Getenv("SIP_ALLOW_FROM"))
	if len(allow) == 0 {
		allow = []string{"0.0.0.0/0"}
	}
	domain := strings.TrimSpace(os.Getenv("SIP_DOMAIN"))
	if domain == "" {
		domain = "sml-development.local"
	}
	user := strings.TrimSpace(os.Getenv("SIP_USER"))
	if user == "" {
		user = "receptionist"
	}
	name := strings.TrimSpace(os.Getenv("SIP_DISPLAY_NAME"))
	if name == "" {
		name = "SML Development"
	}
	return listenConfig{
		ListenIP:    strings.TrimSpace(os.Getenv("SIP_LISTEN_IP")),
		Port:        port,
		Domain:      domain,
		User:        user,
		DisplayName: name,
		AllowFrom:   allow,
		AdvertiseIP: strings.TrimSpace(os.Getenv("SIP_ADVERTISE_IP")),
	}, nil
}

func (c listenConfig) endpoint() sip.Config {
	mode := sip.NATBehindSBC
	if c.AdvertiseIP != "" {
		mode = sip.NATStandalone
	}
	return sip.Config{
		NATMode:     mode,
		ListenIP:    c.ListenIP,
		AdvertiseIP: c.AdvertiseIP,
		Domain:      c.Domain,
		User:        c.User,
		DisplayName: c.DisplayName,
		SIPPort:     c.Port,
		Transports:  []string{"udp"},
		AllowFrom:   c.AllowFrom,
		Codecs:      []sip.Codec{sip.CodecPCMU, sip.CodecPCMA},
	}
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// serve accepts calls until ctx is canceled or the endpoint closes.
// Cancel closes the endpoint so Accept stops waiting.
func serve(ctx context.Context, ep *sip.Endpoint, speech *geminiClient) error {
	go func() {
		<-ctx.Done()
		_ = ep.Close()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		call, err := ep.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			if err := runCall(ctx, call, speech); err != nil {
				slog.Error("call", "err", err)
			}
		}()
	}
}
