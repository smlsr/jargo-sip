package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gojargo/jargo/audio/resample"
	"github.com/gojargo/jargo/frames"
	"github.com/gojargo/jargo/service/stt"
	"github.com/gojargo/jargo/service/tts"
)

const (
	geminiAPI       = "https://generativelanguage.googleapis.com/v1beta"
	pipelineInRate  = 16000
	pipelineOutRate = 24000
	// maxSegment is how much caller audio one recognize request will send.
	maxSegment = 30 * time.Second
)

// geminiClient talks to the Gemini API with one key. Cloud Speech-to-Text and
// Cloud Text-to-Speech both refuse this key, so recognition is generateContent
// audio-in and speech is the Gemini TTS model.
type geminiClient struct {
	key   string
	model string
	voice string
	http  *http.Client
	mu    sync.Mutex
}

func newGemini(key, dialogModel string) *geminiClient {
	return &geminiClient{
		key:   key,
		model: dialogModel,
		voice: voiceName,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (g *geminiClient) newSTT() *stt.SegmentService {
	return stt.NewSegment("GeminiSTT", recognizer{g}, 0)
}

func (g *geminiClient) newTTS() *tts.Base {
	return tts.New("GeminiTTS", speaker{g})
}

// recognizer is the audio-in side. It is its own type so its Metadata
// method does not clash with the synthesizer's.
type recognizer struct{ *geminiClient }

func (r recognizer) Metadata() stt.Metadata {
	return stt.Metadata{Model: r.model}
}

func (r recognizer) Transcribe(ctx context.Context, audio []byte, sampleRate int) (string, error) {
	if sampleRate <= 0 || len(audio) < 2 {
		return "", nil
	}
	maxBytes := int(maxSegment/time.Second) * sampleRate * 2
	if maxBytes > 0 && len(audio) > maxBytes {
		audio = audio[len(audio)-maxBytes:]
	}
	wav, err := pcm16WAV(audio, sampleRate)
	if err != nil {
		return "", err
	}
	body, err := asrBody(wav, r.model)
	if err != nil {
		return "", err
	}
	out, err := r.generate(ctx, r.model, body)
	if err != nil {
		slog.Error("recognize", "err", err)
		var se *statusError
		if errors.As(err, &se) && (se.code == http.StatusUnauthorized || se.code == http.StatusForbidden) {
			return "", err
		}
		return "", nil
	}
	return transcriptText(out), nil
}

// speaker is the audio-out side.
type speaker struct{ *geminiClient }

func (s speaker) Metadata() tts.Metadata {
	return tts.Metadata{Model: ttsModel, VoiceID: s.currentVoice()}
}

func (g *geminiClient) currentVoice() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.voice == "" {
		return voiceName
	}
	return g.voice
}

func (s speaker) SampleRate() int { return pipelineOutRate }

func (s speaker) RunTTS(ctx context.Context, text, _ string, yield func(frames.Frame) error) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	emit := tts.PCMYielder(yield, s.SampleRate())
	pcm, err := s.synthesize(ctx, text, s.currentVoice())
	if err != nil && s.currentVoice() != fallbackVoiceName && voiceRejected(err) {
		slog.Warn("british voice rejected, using the australian voice", "voice", fallbackVoiceName)
		s.mu.Lock()
		s.voice = fallbackVoiceName
		s.mu.Unlock()
		pcm, err = s.synthesize(ctx, text, fallbackVoiceName)
	}
	if err != nil {
		return err
	}
	return emit(pcm)
}

func (g *geminiClient) synthesize(ctx context.Context, text, voice string) ([]byte, error) {
	body, err := ttsBody(text, voice)
	if err != nil {
		return nil, err
	}
	out, err := g.generate(ctx, ttsModel, body)
	if err != nil {
		return nil, err
	}
	raw, mime, err := audioPart(out)
	if err != nil {
		return nil, err
	}
	pcm, rate, err := decodeSpeech(raw, mime)
	if err != nil {
		return nil, err
	}
	if rate == pipelineOutRate {
		return pcm, nil
	}
	rs, err := resample.New(rate, pipelineOutRate, 1)
	if err != nil {
		return nil, err
	}
	return rs.Process(pcm), nil
}

func (g *geminiClient) generate(ctx context.Context, model string, body []byte) ([]byte, error) {
	url := geminiAPI + "/models/" + model + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.key)
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{code: resp.StatusCode, body: clip(buf)}
	}
	return buf, nil
}

func (g *geminiClient) listModels(ctx context.Context) ([]string, error) {
	var names []string
	page := ""
	for range 20 {
		q := url.Values{"pageSize": {"100"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, geminiAPI+"/models?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-goog-api-key", g.key)
		resp, err := g.http.Do(req)
		if err != nil {
			return nil, err
		}
		buf, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &statusError{code: resp.StatusCode, body: clip(buf)}
		}
		var parsed struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
			NextSnake     string `json:"next_page_token"`
		}
		if err := json.Unmarshal(buf, &parsed); err != nil {
			return nil, err
		}
		for _, m := range parsed.Models {
			names = append(names, m.Name)
		}
		page = parsed.NextPageToken
		if page == "" {
			page = parsed.NextSnake
		}
		if page == "" {
			return names, nil
		}
	}
	return names, nil
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("gemini status %d: %s", e.code, e.body)
}

func voiceRejected(err error) bool {
	se, ok := err.(*statusError)
	return ok && se.code == http.StatusBadRequest && strings.Contains(strings.ToLower(se.body), "voice")
}

func asrBody(wav []byte, model string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"contents": []any{map[string]any{
			"role": "user",
			"parts": []any{
				map[string]any{"text": "Transcribe the caller's English speech. Return only the words they said. If there is no speech, return an empty string."},
				map[string]any{"inline_data": map[string]any{
					"mime_type": "audio/wav",
					"data":      base64.StdEncoding.EncodeToString(wav),
				}},
			},
		}},
		"generationConfig": map[string]any{
			"temperature":     0,
			"maxOutputTokens": 200,
			"thinkingConfig":  recognitionThinking(model),
		},
	})
}

func speechLanguage(voice string) string {
	if strings.HasPrefix(voice, "en-au") {
		return "en-AU"
	}
	return "en-GB"
}

func ttsBody(text, voice string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"contents": []any{map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": text}},
		}},
		"generationConfig": map[string]any{
			"responseModalities": []string{"AUDIO"},
			"speechConfig": map[string]any{
				"languageCode": speechLanguage(voice),
				"voiceConfig": map[string]any{
					"prebuiltVoiceConfig": map[string]any{"voiceName": voice},
				},
			},
		},
	})
}

type genResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text    string `json:"text"`
				Thought bool   `json:"thought"`
				Inline  *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func transcriptText(raw []byte) string {
	var out genResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range out.Candidates {
		for _, p := range c.Content.Parts {
			if p.Thought || strings.TrimSpace(p.Text) == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(strings.TrimSpace(p.Text))
		}
	}
	text := strings.Trim(b.String(), `"' `)
	switch strings.ToLower(text) {
	case "", "empty", "[silence]", "(silence)", "no speech":
		return ""
	default:
		return text
	}
}

func audioPart(raw []byte) ([]byte, string, error) {
	var out genResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "", err
	}
	for _, c := range out.Candidates {
		for _, p := range c.Content.Parts {
			if p.Inline == nil || p.Inline.Data == "" {
				continue
			}
			buf, err := base64.StdEncoding.DecodeString(p.Inline.Data)
			if err != nil {
				return nil, "", err
			}
			return buf, p.Inline.MimeType, nil
		}
	}
	return nil, "", fmt.Errorf("gemini tts returned no audio")
}

func decodeSpeech(raw []byte, mime string) ([]byte, int, error) {
	if strings.HasPrefix(mime, "audio/wav") || (len(raw) >= 4 && string(raw[:4]) == "RIFF") {
		return wavPCM(raw)
	}
	rate := pipelineOutRate
	if i := strings.Index(mime, "rate="); i >= 0 {
		fmt.Sscanf(mime[i+5:], "%d", &rate)
	}
	if len(raw)%2 != 0 {
		raw = raw[:len(raw)-1]
	}
	return raw, rate, nil
}

func clip(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// loadEnvFiles fills empty environment variables from a dotenv file. A value
// already set in the process is left as it is. The key never has a default.
func loadEnvFiles() {
	for _, path := range []string{"examples/receptionist/.env", ".env"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		applyEnv(f)
		_ = f.Close()
	}
}

func applyEnv(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		_ = os.Setenv(key, val)
	}
}
