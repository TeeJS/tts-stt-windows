// gen-catalog builds internal/models/catalog.json from the sherpa-onnx GitHub releases, so the
// app ships a complete, browsable model list without hand-maintaining ~1000 asset names.
//
//	go run ./tools/gen-catalog
//
// Run it when sherpa-onnx publishes new models. The generated file is embedded in the binary, so
// browsing and filtering work offline; only the actual download needs the network.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
)

const (
	ttsRelease = "https://api.github.com/repos/k2-fsa/sherpa-onnx/releases/tags/tts-models"
	asrRelease = "https://api.github.com/repos/k2-fsa/sherpa-onnx/releases/tags/asr-models"
)

// Entry is one selectable model in the catalog. Kept deliberately flat — the settings UI renders
// straight from these fields.
type Entry struct {
	ID      string   `json:"id"`                // stable key, also the config value
	Kind    string   `json:"kind"`              // "tts" | "stt"
	Family  string   `json:"family"`            // piper, coqui, whisper, parakeet, sense-voice, moonshine, dolphin
	Name    string   `json:"name"`              // human label, e.g. "Alan" or "Whisper small"
	Langs   []string `json:"langs"`             // PRIMARY codes only, so the filter never splits de/de-DE: ["de"], ["zh","en",…], ["multi"]
	Region  string   `json:"region,omitempty"`  // display-only accent/region, e.g. "GB" — never used for filtering
	Quality string   `json:"quality,omitempty"` // piper/coqui: x_low|low|medium|high
	Size    int64    `json:"size"`              // archive bytes
	URL     string   `json:"url"`
	Dir     string   `json:"dir"` // directory the archive extracts to
	Notes   string   `json:"notes,omitempty"`
}

// diarEntries are the meeting-diarization models: static, because two hand-picked
// assets don't justify scraping a third release. The embedding model choice is
// deliberate — see internal/diarize/diarize.go: eres2net won a measured shootout
// (wespeaker resnet34 doesn't discriminate on real recordings; titanet costs 4x
// the download for no gain). The typo in "speaker-recongition-models" is real.
func diarEntries() []Entry {
	return []Entry{
		{
			ID: "pyannote-segmentation-3-0", Kind: "diar", Family: "segmentation",
			Name: "Pyannote segmentation 3.0", Langs: []string{"multi"}, Size: 6958444,
			URL:   "https://github.com/k2-fsa/sherpa-onnx/releases/download/speaker-segmentation-models/sherpa-onnx-pyannote-segmentation-3-0.tar.bz2",
			Dir:   "sherpa-onnx-pyannote-segmentation-3-0",
			Notes: "Splits a meeting recording into speaker turns.",
		},
		{
			ID: "eres2net-en-voxceleb", Kind: "diar", Family: "embedding",
			Name: "ERes2Net speaker embedding", Langs: []string{"multi"}, Size: 26485263,
			URL:   "https://github.com/k2-fsa/sherpa-onnx/releases/download/speaker-recongition-models/3dspeaker_speech_eres2net_sv_en_voxceleb_16k.onnx",
			Dir:   "eres2net-en-voxceleb",
			Notes: "Voice fingerprints for recognizing enrolled speakers.",
		},
	}
}

type ghRelease struct {
	Assets []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func main() {
	tts := fetch(ttsRelease)
	asr := fetch(asrRelease)

	var entries []Entry
	entries = append(entries, parseTTS(tts)...)
	entries = append(entries, parseSTT(asr)...)
	entries = append(entries, diarEntries()...)

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		if entries[i].Family != entries[j].Family {
			return entries[i].Family < entries[j].Family
		}
		return entries[i].ID < entries[j].ID
	})

	// Language names for the filter dropdown, restricted to codes actually present so a stale
	// entry in the table can't add a language nothing can be downloaded in.
	names := map[string]string{}
	for _, e := range entries {
		for _, l := range e.Langs {
			if n, ok := langNames[l]; ok {
				names[l] = n
			} else {
				names[l] = l // surfaces a missing table entry in the UI instead of hiding the models
				log.Printf("warning: no display name for language code %q", l)
			}
		}
	}

	out := struct {
		Generated string            `json:"generated"`
		Languages map[string]string `json:"languages"`
		Entries   []Entry           `json:"entries"`
	}{Generated: "sherpa-onnx tts-models + asr-models releases", Languages: names, Entries: entries}

	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("internal/models/catalog.json", append(b, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}

	var tCount, sCount int
	langs := map[string]bool{}
	for _, e := range entries {
		if e.Kind == "tts" {
			tCount++
		} else {
			sCount++
		}
		for _, l := range e.Langs {
			langs[l] = true
		}
	}
	log.Printf("wrote internal/models/catalog.json: %d voices, %d speech models, %d languages", tCount, sCount, len(langs))
}

func fetch(url string) ghRelease {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok) // optional: dodges the 60/hr anonymous limit
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("fetch %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Fatalf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	var r ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		log.Fatalf("decode %s: %v", url, err)
	}
	return r
}

// ---- text-to-speech ----

// piper assets are `vits-piper-<locale>-<voice>-<quality>[-int8|-fp16].tar.bz2`. The quantized
// variants are separate archives; we keep int8 (smaller, no audible quality cost) and drop the
// fp32 twin so the list shows one card per voice.
var piperRe = regexp.MustCompile(`^vits-piper-([a-z]{2,3}(?:_[A-Za-z]{2,3})?)-(.+?)-(x_low|low|medium|high)(-int8|-fp16)?\.tar\.bz2$`)

// coqui assets are `vits-coqui-<lang>-<dataset>[-...]` — no quality tier, one per language/dataset.
var coquiRe = regexp.MustCompile(`^vits-coqui-([a-z]{2,3})-(.+)\.tar\.bz2$`)

func parseTTS(r ghRelease) []Entry {
	// Collect piper voices keyed by locale+voice+quality so int8/fp32/fp16 collapse to one entry.
	type variant struct {
		url  string
		size int64
		dir  string
	}
	piper := map[string]map[string]variant{} // key -> variantName -> data
	meta := map[string][3]string{}           // key -> {locale, voice, quality}
	var entries []Entry

	for _, a := range r.Assets {
		if !strings.HasSuffix(a.Name, ".tar.bz2") {
			continue
		}
		if isHardwareSpecific(a.Name) {
			continue // other vendors' NPU builds, not usable on desktop CPU
		}
		if m := piperRe.FindStringSubmatch(a.Name); m != nil {
			locale, voice, quality, suffix := m[1], m[2], m[3], m[4]
			key := locale + "|" + voice + "|" + quality
			if piper[key] == nil {
				piper[key] = map[string]variant{}
				meta[key] = [3]string{locale, voice, quality}
			}
			vName := "fp32"
			switch suffix {
			case "-int8":
				vName = "int8"
			case "-fp16":
				vName = "fp16"
			}
			piper[key][vName] = variant{a.BrowserDownloadURL, a.Size, strings.TrimSuffix(a.Name, ".tar.bz2")}
			continue
		}
		if m := coquiRe.FindStringSubmatch(a.Name); m != nil {
			lang, dataset := m[1], m[2]
			entries = append(entries, Entry{
				ID:     "coqui-" + lang + "-" + dataset,
				Kind:   "tts",
				Family: "coqui",
				Name:   prettyDataset(dataset),
				Langs:  []string{lang},
				Size:   a.Size,
				URL:    a.BrowserDownloadURL,
				Dir:    strings.TrimSuffix(a.Name, ".tar.bz2"),
			})
		}
	}

	for key, vars := range piper {
		m := meta[key]
		locale, voice, quality := m[0], m[1], m[2]
		// Prefer int8 (smaller download, same audible quality), else fp32, else fp16.
		v, ok := vars["int8"]
		if !ok {
			if v, ok = vars["fp32"]; !ok {
				v = vars["fp16"]
			}
		}
		lang, region := splitLocale(locale)
		entries = append(entries, Entry{
			ID:      "piper-" + strings.ReplaceAll(locale, "_", "-") + "-" + voice + "-" + quality,
			Kind:    "tts",
			Family:  "piper",
			Name:    prettyVoice(voice),
			Langs:   []string{lang},
			Region:  region,
			Quality: quality,
			Size:    v.size,
			URL:     v.url,
			Dir:     v.dir,
		})
	}
	return entries
}

// ---- speech-to-text ----

// Asset names don't encode language reliably across ASR families, so language is assigned by
// rule per family. Only offline (non-streaming) models our engine can load are included.
func parseSTT(r ghRelease) []Entry {
	var entries []Entry
	add := func(e Entry) { entries = append(entries, e) }

	for _, a := range r.Assets {
		n := strings.TrimSuffix(a.Name, ".tar.bz2")
		if !strings.HasSuffix(a.Name, ".tar.bz2") {
			continue
		}
		if isHardwareSpecific(n) || strings.Contains(n, "streaming") {
			continue // other vendors' NPU builds, and streaming models: not what this server runs
		}
		base := Entry{Kind: "stt", Size: a.Size, URL: a.BrowserDownloadURL, Dir: n}

		switch {
		case strings.Contains(n, "whisper"):
			e := base
			e.Family = "whisper"
			name := strings.TrimPrefix(n, "sherpa-onnx-whisper-")
			if strings.Contains(n, "aishell") {
				continue // fine-tuned for one Chinese dataset; the general models serve better
			}
			e.ID = "whisper-" + name
			e.Name = "Whisper " + strings.ReplaceAll(name, ".en", " (English)")
			switch {
			case strings.HasSuffix(name, ".en"):
				e.Langs = []string{"en"}
			case strings.Contains(name, "distil"):
				// Every Distil-Whisper checkpoint is distilled on English data only — including the
				// ones without a .en suffix (distil-large-v2/v3/v3.5). Fed foreign speech they emit
				// a rough English rendering, which looks multilingual but isn't (bit a real user).
				e.Langs = []string{"en"}
				e.Notes = "English only (distilled)"
			default:
				e.Langs = []string{"multi"}
				e.Notes = "99 languages"
			}
			add(e)

		case strings.Contains(n, "parakeet"):
			e := base
			e.Family = "parakeet"
			e.ID = strings.TrimPrefix(n, "sherpa-onnx-nemo-")
			switch {
			case strings.Contains(n, "-v3"):
				e.Name = "Parakeet v3"
				e.Langs = []string{"multi"}
				e.Notes = "25 European languages · fast"
			case strings.Contains(n, "-v2"):
				e.Name = "Parakeet v2"
				e.Langs = []string{"en"}
				e.Notes = "fast"
			case strings.Contains(n, "-ja-"):
				e.Name = "Parakeet (Japanese)"
				e.Langs = []string{"ja"}
			default:
				e.Name = "Parakeet 110M"
				e.Langs = []string{"en"}
			}
			add(e)

		case strings.Contains(n, "sense-voice"):
			e := base
			e.Family = "sense-voice"
			e.ID = strings.TrimPrefix(n, "sherpa-onnx-")
			e.Name = "SenseVoice"
			e.Langs = []string{"zh", "en", "ja", "ko", "yue"}
			add(e)

		case strings.Contains(n, "moonshine"):
			e := base
			e.Family = "moonshine"
			e.ID = strings.TrimPrefix(n, "sherpa-onnx-")
			parts := strings.Split(strings.TrimPrefix(n, "sherpa-onnx-moonshine-"), "-")
			if len(parts) < 2 {
				continue
			}
			size, lang := parts[0], parts[1]
			e.Name = "Moonshine " + size
			e.Langs = []string{lang}
			e.Notes = "very fast"
			add(e)

		case strings.Contains(n, "dolphin"):
			e := base
			e.Family = "dolphin"
			e.ID = strings.TrimPrefix(n, "sherpa-onnx-")
			e.Name = "Dolphin " + pick(strings.Contains(n, "small"), "small", "base")
			e.Langs = []string{"multi"}
			e.Notes = "40 Asian languages"
			add(e)
		}
	}

	// One card per model. The same model ships as fp32/int8/quantized builds and is re-released
	// under new dates (e.g. Moonshine base en: a 239MB 2024 build and a 106MB 2026 rebuild), so
	// collapse on the date- and quantization-stripped name and keep the best: newest release
	// first, then the quantized build (smaller download, same accuracy in practice).
	byBase := map[string]Entry{}
	dates := map[string]string{}
	for _, e := range entries {
		date := dateRe.FindString(e.ID)
		key := dateRe.ReplaceAllString(e.ID, "")
		key = strings.NewReplacer("-int8", "", "-fp16", "", "-quantized", "").Replace(key)
		key = strings.Trim(key, "-")
		prev, seen := byBase[key]
		if !seen || better(date, dates[key], e.ID, prev.ID) {
			e.ID = key
			byBase[key] = e
			dates[key] = date
		}
	}
	out := make([]Entry, 0, len(byBase))
	for _, e := range byBase {
		out = append(out, e)
	}
	return out
}

var dateRe = regexp.MustCompile(`-?\d{4}-\d{2}-\d{2}`)

// Builds compiled for other vendors' accelerators or for phones — Rockchip (rk3562/3566/3576/3588,
// rknn), Huawei Ascend/CANN, Qualcomm QNN, and the fixed-input-duration variants those require.
// They can't run on a desktop CPU, and they otherwise outnumber the usable models in the list.
var hardwareRe = regexp.MustCompile(`rk\d{4}|rknn|ascend|cann|qnn|android|aarch64|\d+-seconds`)

func isHardwareSpecific(name string) bool { return hardwareRe.MatchString(strings.ToLower(name)) }

// Display names for every language code the catalog can produce. English endonyms are avoided in
// favor of the names an English-speaking UI shows; "multi" marks models that handle many
// languages at once (Whisper, Parakeet v3, Dolphin, SenseVoice).
var langNames = map[string]string{
	"ar": "Arabic", "bg": "Bulgarian", "bn": "Bengali", "ca": "Catalan", "cs": "Czech",
	"cy": "Welsh", "da": "Danish", "de": "German", "el": "Greek", "en": "English",
	"es": "Spanish", "et": "Estonian", "eu": "Basque", "fa": "Persian", "fi": "Finnish",
	"fr": "French", "ga": "Irish", "hi": "Hindi", "hr": "Croatian", "hu": "Hungarian",
	"id": "Indonesian", "is": "Icelandic", "it": "Italian", "ja": "Japanese", "ka": "Georgian",
	"kk": "Kazakh", "ko": "Korean", "ku": "Kurdish", "lb": "Luxembourgish", "lt": "Lithuanian",
	"lv": "Latvian", "ml": "Malayalam", "mt": "Maltese", "multi": "Multi-language",
	"ne": "Nepali", "nl": "Dutch", "no": "Norwegian", "pl": "Polish", "pt": "Portuguese",
	"ro": "Romanian", "ru": "Russian", "sk": "Slovak", "sl": "Slovenian", "sq": "Albanian",
	"sr": "Serbian", "sv": "Swedish", "sw": "Swahili", "tr": "Turkish", "uk": "Ukrainian",
	"ur": "Urdu", "vi": "Vietnamese", "yue": "Cantonese", "zh": "Chinese",
}

// better reports whether candidate (date, id) should replace the incumbent: a newer release date
// wins outright; at the same date, a quantized build wins.
func better(date, prevDate, id, prevID string) bool {
	if date != prevDate {
		return date > prevDate // ISO dates sort lexically; "" (undated) loses to any dated build
	}
	quant := func(s string) bool {
		return strings.Contains(s, "int8") || strings.Contains(s, "quantized")
	}
	return quant(id) && !quant(prevID)
}

// ---- helpers ----

// prettyDataset labels a Coqui voice. Coqui models are named after the corpus they were trained
// on rather than a person, and the raw slugs ("cv", "css10", "mai_female") mean nothing to a user
// scanning the list, so the known corpus names are spelled out.
func prettyDataset(s string) string {
	switch strings.ToLower(s) {
	case "cv":
		return "Common Voice"
	case "css10":
		return "CSS10"
	case "ljspeech":
		return "LJSpeech"
	case "ljspeech-neon":
		return "LJSpeech (Neon)"
	case "vctk":
		return "VCTK (multi-speaker)"
	case "mai_female":
		return "MAI (female)"
	case "mai_male":
		return "MAI (male)"
	case "custom_female":
		return "Custom (female)"
	case "custom_male":
		return "Custom (male)"
	}
	return prettyVoice(s)
}

// prettyVoice turns an asset's voice slug into a readable label:
// "northern_english_male" -> "Northern English Male", "jenny_dioco" -> "Jenny Dioco".
func prettyVoice(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// splitLocale separates piper's "en_GB" into the primary language ("en") and a display-only
// region ("GB"). Filtering uses the language alone, so a German speaker sees de_DE piper voices
// and bare-"de" coqui voices in one list instead of two near-identical filter options. The
// bilingual oddball "fa_en" (Persian content with English words) reduces to plain "fa".
func splitLocale(l string) (lang, region string) {
	i := strings.Index(l, "_")
	if i < 0 {
		return l, ""
	}
	r := l[i+1:]
	if len(r) == 2 && strings.ToUpper(r) == r {
		return l[:i], r
	}
	return l[:i], ""
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

var _ = fmt.Sprintf
