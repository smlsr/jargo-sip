package main

import (
	"os"
	"strings"
	"testing"
)

func TestEnvParse(t *testing.T) {
	const src = "# comment\n\nGEMINI_API_KEY=\nSIP_PORT=5062\nSIP_ALLOW_FROM=\"10.0.0.1, 10.0.0.2\"\n"
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("SIP_PORT", "")
	t.Setenv("SIP_ALLOW_FROM", "")
	applyEnv(strings.NewReader(src))
	if os.Getenv("SIP_PORT") != "5062" {
		t.Fatalf("port %q", os.Getenv("SIP_PORT"))
	}
	if os.Getenv("GEMINI_API_KEY") != "" {
		t.Fatal("empty assignment stored a key")
	}
}

func TestListenDefaults(t *testing.T) {
	t.Setenv("SIP_PORT", "")
	t.Setenv("SIP_DOMAIN", "")
	t.Setenv("SIP_USER", "")
	t.Setenv("SIP_DISPLAY_NAME", "")
	t.Setenv("SIP_ALLOW_FROM", "")
	t.Setenv("SIP_ADVERTISE_IP", "")
	t.Setenv("SIP_LISTEN_IP", "")
	cfg, err := listenConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 5060 || cfg.Domain != "sml-development.local" || cfg.User != "receptionist" {
		t.Fatalf("%+v", cfg)
	}
	if len(cfg.AllowFrom) != 1 || cfg.AllowFrom[0] != "0.0.0.0/0" {
		t.Fatalf("allow %v", cfg.AllowFrom)
	}
	ep := cfg.endpoint()
	if ep.DisplayName != "SML Development" {
		t.Fatalf("name %s", ep.DisplayName)
	}
	if len(ep.Codecs) != 2 || ep.Codecs[0] != "PCMU" || ep.Codecs[1] != "PCMA" {
		t.Fatalf("codecs %v", ep.Codecs)
	}
}

func TestTTSBodyNamesTheBritishVoice(t *testing.T) {
	body, err := ttsBody("Hello.", voiceName)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, voiceName) || !strings.Contains(text, "en-GB") {
		t.Fatalf("body %s", text)
	}
	if strings.Contains(text, "AQ.") {
		t.Fatal("request body contains a key")
	}
	au, err := ttsBody("Hello.", fallbackVoiceName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(au), "en-AU") {
		t.Fatalf("australian body %s", au)
	}
}

func TestEnvExampleHasNoSecret(t *testing.T) {
	b, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, "GEMINI_API_KEY=") {
		t.Fatal("example is missing GEMINI_API_KEY")
	}
	if strings.Contains(text, "AQ.") {
		t.Fatal("example contains a key")
	}
}

func TestTranscriptText(t *testing.T) {
	raw := []byte(`{"candidates":[{"content":{"parts":[{"text":"  Do you build custom homes?  "}]}}]}`)
	if got := transcriptText(raw); got != "Do you build custom homes?" {
		t.Fatalf("%q", got)
	}
	raw = []byte(`{"candidates":[{"content":{"parts":[{"text":"(silence)"}]}}]}`)
	if got := transcriptText(raw); got != "" {
		t.Fatalf("%q", got)
	}
}
