package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/pii"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// mockTranscribeServer mimics the audio sidecar's POST /transcribe:
// accepts the multipart upload, asserts an audio part is present, and
// returns a diarized transcript.
func mockTranscribeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transcribe" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
			return
		}
		f, hdr, err := r.FormFile("audio")
		if err != nil {
			http.Error(w, "missing audio field", http.StatusBadRequest)
			return
		}
		f.Close()
		if hdr.Filename == "" {
			http.Error(w, "empty filename", http.StatusBadRequest)
			return
		}
		diarize := r.FormValue("diarize") == "true"

		mkWord := func(word string, start, end float64, speaker string) map[string]any {
			m := map[string]any{"word": word, "start": start, "end": end}
			if speaker != "" {
				m["speaker"] = speaker
			}
			return m
		}
		words := []map[string]any{
			mkWord("Hello", 0.0, 0.4, ""),
			mkWord("there", 0.5, 0.9, ""),
		}
		segments := []map[string]any{
			{"start": 0.0, "end": 1.0, "text": "Hello there."},
		}
		if diarize {
			// Real sidecar batch labels (Sortformer): uppercase, zero-padded.
			words[0]["speaker"] = "SPEAKER_00"
			words[1]["speaker"] = "SPEAKER_01"
			segments[0]["speaker"] = "SPEAKER_00"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"text":               "Hello there.",
			"duration":           1.0,
			"processing_time_ms": 42,
			"model":              "parakeet-tdt-v3-coreml",
			"diarized":           diarize,
			"itn_applied":        true,
			"words":              words,
			"segments":           segments,
		})
	}))
}

func dgPreRecordedTestApp(t *testing.T, sidecarURL string) *fiber.App {
	t.Helper()
	sc := sidecar.NewClient(sidecarURL, "ws://unused", "http://unused-llm")
	h := NewDeepgramPreRecordedHandler(sc, pii.NewRedactor(nil, false, nil, 0), true, captureLogManager())
	app := fiber.New()
	app.Post("/v1/listen", h.Listen)
	return app
}

// TestDeepgramPreRecordedRawBody verifies the Deepgram ListenV1Response
// shape for a raw-audio POST: spec-required metadata fields, channel
// alternatives with words, and usage locals.
func TestDeepgramPreRecordedRawBody(t *testing.T) {
	sidecarHTTP := mockTranscribeServer(t)
	defer sidecarHTTP.Close()
	app := dgPreRecordedTestApp(t, sidecarHTTP.URL)

	req := httptest.NewRequest("POST", "/v1/listen?language=en", strings.NewReader("fake-pcm-bytes"))
	req.Header.Set("Content-Type", "audio/wav")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// metadata — spec-required fields.
	md, _ := body["metadata"].(map[string]any)
	if md == nil {
		t.Fatal("missing metadata")
	}
	if md["transaction_key"] != "deprecated" {
		t.Errorf("transaction_key = %v, want deprecated", md["transaction_key"])
	}
	if rid, _ := md["request_id"].(string); rid == "" {
		t.Error("metadata.request_id missing")
	}
	if s, _ := md["sha256"].(string); len(s) != 64 {
		t.Errorf("sha256 = %q, want 64-char hex", s)
	}
	if d, _ := md["duration"].(float64); d != 1.0 {
		t.Errorf("duration = %v, want 1.0", d)
	}
	if ch, _ := md["channels"].(float64); ch != 1 {
		t.Errorf("channels = %v, want 1", ch)
	}
	if models, _ := md["models"].([]any); len(models) != 1 {
		t.Errorf("models = %v, want one entry", md["models"])
	}
	mi, _ := md["model_info"].(map[string]any)
	if len(mi) != 1 {
		t.Fatalf("model_info = %v, want one keyed entry", md["model_info"])
	}
	for k, v := range mi {
		entry, _ := v.(map[string]any)
		if entry["name"] != "parakeet-tdt-v3-coreml" || entry["arch"] == "" || entry["version"] == "" {
			t.Errorf("model_info[%q] = %v, missing name/arch/version", k, v)
		}
	}

	// results.channels[0].alternatives[0]
	res, _ := body["results"].(map[string]any)
	chs, _ := res["channels"].([]any)
	if len(chs) != 1 {
		t.Fatalf("channels = %v, want 1", chs)
	}
	ch0, _ := chs[0].(map[string]any)
	alts, _ := ch0["alternatives"].([]any)
	if len(alts) != 1 {
		t.Fatalf("alternatives = %v, want 1", alts)
	}
	alt0, _ := alts[0].(map[string]any)
	if alt0["transcript"] != "Hello there." {
		t.Errorf("transcript = %v, want %q", alt0["transcript"], "Hello there.")
	}
	if alt0["confidence"] != 0.99 {
		t.Errorf("confidence = %v, want 0.99", alt0["confidence"])
	}
	words, _ := alt0["words"].([]any)
	if len(words) != 2 {
		t.Fatalf("words = %v, want 2", words)
	}
	w0, _ := words[0].(map[string]any)
	if w0["word"] != "Hello" || w0["punctuated_word"] != "Hello" {
		t.Errorf("word[0] = %v, want word+punctuated_word", w0)
	}
	if _, present := w0["speaker"]; present {
		t.Errorf("speaker present without diarize: %v", w0["speaker"])
	}

	// No utterances without ?utterances=true.
	if _, present := res["utterances"]; present {
		t.Errorf("utterances present without request: %v", res["utterances"])
	}
	if _, present := ch0["detected_language"]; present {
		t.Errorf("detected_language present without request: %v", ch0["detected_language"])
	}
}

// TestDeepgramPreRecordedDiarizeUtterances verifies speaker IDs map to
// Deepgram's integer form and utterances=true yields spec-shaped
// utterances with per-utterance words.
func TestDeepgramPreRecordedDiarizeUtterances(t *testing.T) {
	sidecarHTTP := mockTranscribeServer(t)
	defer sidecarHTTP.Close()
	app := dgPreRecordedTestApp(t, sidecarHTTP.URL)

	req := httptest.NewRequest("POST", "/v1/listen?diarize=true&utterances=true&detect_language=true",
		strings.NewReader("fake-pcm-bytes"))
	req.Header.Set("Content-Type", "audio/wav")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	res, _ := body["results"].(map[string]any)
	chs, _ := res["channels"].([]any)
	ch0, _ := chs[0].(map[string]any)
	if dl, _ := ch0["detected_language"].(string); dl != "en" {
		t.Errorf("detected_language = %v, want en", dl)
	}
	alts, _ := ch0["alternatives"].([]any)
	alt0, _ := alts[0].(map[string]any)
	words, _ := alt0["words"].([]any)
	w0, _ := words[0].(map[string]any)
	if sp, _ := w0["speaker"].(float64); sp != 0 {
		t.Errorf("word[0].speaker = %v, want 0 (integer)", w0["speaker"])
	}
	w1, _ := words[1].(map[string]any)
	if sp, _ := w1["speaker"].(float64); sp != 1 {
		t.Errorf("word[1].speaker = %v, want 1 (speaker_1 → 1)", w1["speaker"])
	}

	utts, _ := res["utterances"].([]any)
	if len(utts) != 1 {
		t.Fatalf("utterances = %v, want 1", utts)
	}
	u0, _ := utts[0].(map[string]any)
	if u0["transcript"] != "Hello there." {
		t.Errorf("utterance transcript = %v", u0["transcript"])
	}
	if u0["id"] != "utt-001" {
		t.Errorf("utterance id = %v, want utt-001", u0["id"])
	}
	if ch, _ := u0["channel"].(float64); ch != 0 {
		t.Errorf("utterance channel = %v, want 0", ch)
	}
	if sp, _ := u0["speaker"].(float64); sp != 0 {
		t.Errorf("utterance speaker = %v, want 0", u0["speaker"])
	}
	uw, _ := u0["words"].([]any)
	if len(uw) != 2 {
		t.Errorf("utterance words = %d, want 2", len(uw))
	}
}

// TestDeepgramPreRecordedURLMode verifies the JSON {"url": …} body mode:
// the proxy fetches the audio server-side and transcribes it.
func TestDeepgramPreRecordedURLMode(t *testing.T) {
	sidecarHTTP := mockTranscribeServer(t)
	defer sidecarHTTP.Close()

	// Audio source server the proxy will fetch from.
	var fetched bool
	audioSrc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched = true
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("remote-audio-bytes"))
	}))
	defer audioSrc.Close()

	app := dgPreRecordedTestApp(t, sidecarHTTP.URL)

	req := httptest.NewRequest("POST", "/v1/listen",
		strings.NewReader(fmt.Sprintf(`{"url":%q}`, audioSrc.URL+"/clip.wav")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !fetched {
		t.Error("proxy never fetched the audio URL")
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	res, _ := body["results"].(map[string]any)
	chs, _ := res["channels"].([]any)
	ch0, _ := chs[0].(map[string]any)
	alts, _ := ch0["alternatives"].([]any)
	alt0, _ := alts[0].(map[string]any)
	if alt0["transcript"] != "Hello there." {
		t.Errorf("transcript = %v, want %q", alt0["transcript"], "Hello there.")
	}
}

// TestDeepgramPreRecordedErrors verifies Deepgram-shaped errors:
// err_code / err_msg / request_id on invalid requests.
func TestDeepgramPreRecordedErrors(t *testing.T) {
	sidecarHTTP := mockTranscribeServer(t)
	defer sidecarHTTP.Close()
	app := dgPreRecordedTestApp(t, sidecarHTTP.URL)

	cases := []struct {
		name       string
		body       string
		ct         string
		wantStatus int
		wantCode   string
	}{
		{"empty raw body", "", "audio/wav", 400, "INVALID_REQUEST"},
		{"json without url", `{"foo":"bar"}`, "application/json", 400, "INVALID_REQUEST"},
		{"bad url scheme", `{"url":"file:///etc/passwd"}`, "application/json", 400, "INVALID_REQUEST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/listen", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.ct)
			resp, err := app.Test(req, -1)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["err_code"] != tc.wantCode {
				t.Errorf("err_code = %v, want %v", body["err_code"], tc.wantCode)
			}
			if body["err_msg"] == nil || body["err_msg"] == "" {
				t.Error("err_msg missing")
			}
			if body["request_id"] == nil || body["request_id"] == "" {
				t.Error("request_id missing")
			}
		})
	}
}

// TestDGSpeakerID verifies sidecar string labels → Deepgram integer IDs.
// The batch route emits Sortformer-style "SPEAKER_00" labels (uppercase,
// zero-padded); the streaming route emits bare indices — both must parse.
func TestDGSpeakerID(t *testing.T) {
	if got := dgSpeakerID(""); got != nil {
		t.Errorf("empty label = %v, want nil", *got)
	}
	if got := dgSpeakerID("0"); got == nil || *got != 0 {
		t.Errorf("'0' = %v, want 0", got)
	}
	if got := dgSpeakerID("speaker_2"); got == nil || *got != 2 {
		t.Errorf("'speaker_2' = %v, want 2", got)
	}
	// Real sidecar batch labels (verified against live /transcribe).
	if got := dgSpeakerID("SPEAKER_00"); got == nil || *got != 0 {
		t.Errorf("'SPEAKER_00' = %v, want 0", got)
	}
	if got := dgSpeakerID("SPEAKER_01"); got == nil || *got != 1 {
		t.Errorf("'SPEAKER_01' = %v, want 1 — uppercase padded labels must parse", got)
	}
}
