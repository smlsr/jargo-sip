package main

import (
	"strings"
	"testing"
)

func TestSelectDialogModelPrefersFlash35(t *testing.T) {
	names := []string{
		"models/gemini-2.5-flash",
		"models/gemini-3-flash-preview",
		"models/gemini-3.5-flash-lite",
		"models/gemini-3.5-flash",
		"models/gemini-3.8-flash",
		"models/gemini-3.8-flash-tts",
		"models/gemini-2.5-flash-image",
		"models/gemini-omni-flash-preview",
	}
	got, err := selectDialogModel(names)
	if err != nil {
		t.Fatal(err)
	}
	if got != "gemini-3.5-flash" {
		t.Fatalf("model %s", got)
	}
}

func TestSelectDialogModelNewestFlash(t *testing.T) {
	names := []string{
		"models/gemini-2.5-flash",
		"models/gemini-3-flash-preview",
		"models/gemini-3.7-flash",
		"models/gemini-3.8-flash-lite",
		"models/gemini-3.8-flash",
		"models/gemini-flash-latest",
		"models/gemini-3.1-flash-tts-preview",
	}
	got, err := selectDialogModel(names)
	if err != nil {
		t.Fatal(err)
	}
	if got != "gemini-3.8-flash" {
		t.Fatalf("model %s", got)
	}
}

func TestSelectDialogModelFlash35Lite(t *testing.T) {
	got, err := selectDialogModel([]string{"models/gemini-3.8-flash", "gemini-3.5-flash-lite"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "gemini-3.5-flash-lite" {
		t.Fatalf("model %s", got)
	}
}

func TestRecognitionThinking(t *testing.T) {
	cases := []struct {
		model string
		key   string
		value any
	}{
		{"gemini-3.5-flash", "thinkingLevel", "minimal"},
		{"models/gemini-3.5-flash", "thinkingLevel", "minimal"},
		{"gemini-3.5-flash-lite", "thinkingLevel", "minimal"},
		{"gemini-3.6-flash", "thinkingLevel", "minimal"},
		{"gemini-3.7-flash", "thinkingLevel", "low"},
		{"gemini-3.8-flash", "thinkingLevel", "low"},
		{"models/gemini-3.8-flash", "thinkingLevel", "low"},
		{"gemini-3.8-flash-lite", "thinkingLevel", "minimal"},
		{"gemini-4-flash", "thinkingLevel", "low"},
		{"gemini-flash-latest", "thinkingLevel", "low"},
		{"gemini-2.5-flash", "thinkingBudget", 0},
	}
	for _, tc := range cases {
		got := recognitionThinking(tc.model)
		if got[tc.key] != tc.value || len(got) != 1 {
			t.Errorf("%s thinking %v", tc.model, got)
		}
	}
}

func TestASRBodyUsesTheModelsThinkingLevel(t *testing.T) {
	flash35, err := asrBody([]byte{0, 0}, "gemini-3.5-flash")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(flash35), `"thinkingLevel":"minimal"`) {
		t.Fatalf("3.5 body %s", flash35)
	}
	flash38, err := asrBody([]byte{0, 0}, "gemini-3.8-flash")
	if err != nil {
		t.Fatal(err)
	}
	text := string(flash38)
	if !strings.Contains(text, `"thinkingLevel":"low"`) || strings.Contains(text, "minimal") {
		t.Fatalf("3.8 body %s", text)
	}
}

func TestSelectDialogModelEmpty(t *testing.T) {
	if _, err := selectDialogModel([]string{"models/gemini-3.8-flash-tts"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestPersona(t *testing.T) {
	for _, want := range []string{
		"SML Development",
		"custom builder",
		"English",
		"end_call",
		"do not take a message",
		"do not offer voicemail",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if endCallTool != "end_call" {
		t.Fatalf("tool %s", endCallTool)
	}
	if voiceName != "en-gb-concierge-2" {
		t.Fatalf("voice %s", voiceName)
	}
	if fallbackVoiceName != "en-au-concierge-3" {
		t.Fatalf("fallback voice %s", fallbackVoiceName)
	}
	if ttsModel != "gemini-3.8-flash-tts" {
		t.Fatalf("tts %s", ttsModel)
	}
}
