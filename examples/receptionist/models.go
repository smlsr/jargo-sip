package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// fallbackDialogModel is the Flash 3.5 id this key's model list returned.
// Startup still reads the live list and applies selectDialogModel.
const fallbackDialogModel = "gemini-3.5-flash"

// ttsModel is the Gemini text-to-speech model. Cloud Text-to-Speech rejects
// an API key, so speech is synthesized here instead.
const ttsModel = "gemini-3.8-flash-tts"

// voiceName is the British female concierge voice from the Gemini voice
// library (en-GB, Winchester English). An Australian female voice is only
// used when this id is rejected.
const voiceName = "en-gb-concierge-2"

// fallbackVoiceName is the Australian female concierge voice.
const fallbackVoiceName = "en-au-concierge-3"

var flashVersion = regexp.MustCompile(`^gemini-(\d+)(?:\.(\d+))?-flash`)

// selectDialogModel picks the dialog model. A Flash 3.5 text model wins when
// the list has one. Otherwise the newest text Flash model in the list wins.
func selectDialogModel(names []string) (string, error) {
	flashes := flashModels(names)
	if len(flashes) == 0 {
		return "", fmt.Errorf("model list has no text Flash model")
	}
	var best35 *flashModel
	for i := range flashes {
		if flashes[i].major == 3 && flashes[i].minor == 5 {
			if best35 == nil || flashNewer(flashes[i], *best35) {
				best35 = &flashes[i]
			}
		}
	}
	if best35 != nil {
		return best35.id, nil
	}
	best := flashes[0]
	for _, f := range flashes[1:] {
		if flashNewer(f, best) {
			best = f
		}
	}
	return best.id, nil
}

// recognitionThinking is the lowest thinking setting a dialog model accepts.
// Gemini 3.5 Flash accepts "minimal". Gemini 3.7 Flash and Gemini 3.8 Flash
// reject "minimal" and accept "low", as do later numbered flashes. Gemini 2.5
// takes a budget, and 0 turns thinking off. An alias can move onto a model
// that rejects "minimal", so it is sent "low".
func recognitionThinking(model string) map[string]any {
	f, ok := parseFlash(model)
	if !ok || f.alias {
		return map[string]any{"thinkingLevel": "low"}
	}
	if f.major < 3 {
		return map[string]any{"thinkingBudget": 0}
	}
	if !f.lite && (f.major > 3 || f.minor >= 7) {
		return map[string]any{"thinkingLevel": "low"}
	}
	return map[string]any{"thinkingLevel": "minimal"}
}

type flashModel struct {
	id      string
	major   int
	minor   int
	preview bool
	lite    bool
	alias   bool
}

func flashModels(names []string) []flashModel {
	var out []flashModel
	seen := map[string]struct{}{}
	for _, raw := range names {
		f, ok := parseFlash(raw)
		if !ok {
			continue
		}
		if _, dup := seen[f.id]; dup {
			continue
		}
		seen[f.id] = struct{}{}
		out = append(out, f)
	}
	return out
}

func parseFlash(raw string) (flashModel, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if id == "" || !strings.Contains(id, "flash") {
		return flashModel{}, false
	}
	for _, skip := range []string{"image", "tts", "live", "audio", "transcribe", "omni", "embedding", "native"} {
		if strings.Contains(id, skip) {
			return flashModel{}, false
		}
	}
	f := flashModel{
		id:      id,
		preview: strings.Contains(id, "preview"),
		lite:    strings.Contains(id, "lite"),
		alias:   strings.Contains(id, "latest"),
	}
	m := flashVersion.FindStringSubmatch(id)
	if m == nil {
		if !f.alias {
			return flashModel{}, false
		}
		return f, true
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return flashModel{}, false
	}
	f.major = major
	if m[2] != "" {
		minor, err := strconv.Atoi(m[2])
		if err != nil {
			return flashModel{}, false
		}
		f.minor = minor
	}
	return f, true
}

// flashNewer reports whether a should be chosen over b when no Flash 3.5
// model is present. A numbered release beats an alias, a full model beats a
// lite or preview of the same version, and a higher version beats a lower one.
func flashNewer(a, b flashModel) bool {
	if a.alias != b.alias {
		return !a.alias
	}
	if a.major != b.major {
		return a.major > b.major
	}
	if a.minor != b.minor {
		return a.minor > b.minor
	}
	if a.preview != b.preview {
		return !a.preview
	}
	if a.lite != b.lite {
		return !a.lite
	}
	return a.id > b.id
}
