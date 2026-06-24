package pii

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// ── helpers ───────────────────────────────────────────────────

// fakePresidio returns an httptest.Server that mimics the
// presidio-analyzer /analyze endpoint. The handler consults the
// supplied entityMap (text → entities) and returns 200 + JSON; if no
// entry matches, returns an empty array.
func fakePresidio(t *testing.T, entityMap map[string][]sidecar.PresidioEntity, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		if r.URL.Path != "/analyze" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body struct {
			Text     string   `json:"text"`
			Language string   `json:"language"`
			Entities []string `json:"entities"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		ents, ok := entityMap[body.Text]
		if !ok {
			ents = nil
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ents)
	}))
}

// ── tests ─────────────────────────────────────────────────────

func TestRedactor_DisabledReturnsInputUnchanged(t *testing.T) {
	r := NewRedactor(nil, false, nil, 0)
	got, items, err := r.RedactText(context.Background(), "Call John at 212-555-1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Call John at 212-555-1234" {
		t.Errorf("disabled redactor should passthrough; got %q", got)
	}
	if items != nil {
		t.Errorf("disabled redactor should return nil items; got %v", items)
	}
}

func TestRedactor_EmptyInputReturnsEmpty(t *testing.T) {
	// Even when enabled, empty input must short-circuit before any HTTP call.
	srv := fakePresidio(t, nil, nil)
	defer srv.Close()

	r := NewRedactor(sidecar.NewPresidioClient(srv.URL, time.Second), true, nil, 0)
	got, _, err := r.RedactText(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string back; got %q", got)
	}
}

func TestRedactor_ReplacesEntitiesRightToLeft(t *testing.T) {
	entities := map[string][]sidecar.PresidioEntity{
		"Call John at 212-555-1234": {
			{Start: 5, End: 9, Score: 0.95, EntityType: "PERSON"},
			{Start: 13, End: 25, Score: 0.98, EntityType: "PHONE_NUMBER"},
		},
	}
	srv := fakePresidio(t, entities, nil)
	defer srv.Close()

	r := NewRedactor(sidecar.NewPresidioClient(srv.URL, time.Second), true, nil, 0)
	got, items, err := r.RedactText(context.Background(), "Call John at 212-555-1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "Call <PERSON> at <PHONE_NUMBER>"
	if got != want {
		t.Errorf("expected %q; got %q", want, got)
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items; got %d", len(items))
	}
}

func TestRedactor_OverlappingEntitiesHandled(t *testing.T) {
	// "John Smith" — Presidio could legitimately return either one
	// PERSON span or two. The replace algorithm should produce
	// sensible output regardless.
	entities := map[string][]sidecar.PresidioEntity{
		"John Smith and Jane Doe": {
			{Start: 0, End: 10, Score: 0.9, EntityType: "PERSON"},
			{Start: 15, End: 23, Score: 0.9, EntityType: "PERSON"},
		},
	}
	srv := fakePresidio(t, entities, nil)
	defer srv.Close()

	r := NewRedactor(sidecar.NewPresidioClient(srv.URL, time.Second), true, nil, 0)
	got, _, err := r.RedactText(context.Background(), "John Smith and Jane Doe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "<PERSON> and <PERSON>"
	if got != want {
		t.Errorf("expected %q; got %q", want, got)
	}
}

func TestRedactor_ScoreThresholdFiltersLowConfidence(t *testing.T) {
	entities := map[string][]sidecar.PresidioEntity{
		"text": {
			{Start: 0, End: 4, Score: 0.95, EntityType: "PERSON"},
			{Start: 5, End: 9, Score: 0.30, EntityType: "LOCATION"}, // below 0.7
		},
	}
	srv := fakePresidio(t, entities, nil)
	defer srv.Close()

	r := NewRedactor(sidecar.NewPresidioClient(srv.URL, time.Second), true, nil, 0.7)
	got, items, _ := r.RedactText(context.Background(), "text")
	if !strings.Contains(got, "<PERSON>") {
		t.Errorf("expected PERSON to survive threshold; got %q", got)
	}
	if strings.Contains(got, "<LOCATION>") {
		t.Errorf("expected LOCATION to be filtered; got %q", got)
	}
	for _, e := range items {
		if e.EntityType == "LOCATION" {
			t.Errorf("filtered entity should not appear in items: %+v", e)
		}
	}
}

func TestRedactor_InvalidOffsetsSkipped(t *testing.T) {
	entities := map[string][]sidecar.PresidioEntity{
		"hello world": {
			{Start: 99, End: 105, Score: 0.9, EntityType: "PERSON"}, // out of range
			{Start: 6, End: 11, Score: 0.9, EntityType: "PERSON"},   // valid
			{Start: 4, End: 4, Score: 0.9, EntityType: "PERSON"},    // zero-width
		},
	}
	srv := fakePresidio(t, entities, nil)
	defer srv.Close()

	r := NewRedactor(sidecar.NewPresidioClient(srv.URL, time.Second), true, nil, 0)
	got, _, err := r.RedactText(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "hello <PERSON>"
	if got != want {
		t.Errorf("expected %q (only valid entity replaced); got %q", want, got)
	}
}

func TestRedactor_FailsClosed_OnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewRedactor(sidecar.NewPresidioClient(srv.URL, time.Second), true, nil, 0)
	got, items, err := r.RedactText(context.Background(), "secret text here")
	if err == nil {
		t.Fatal("expected error from 500 response")
	}
	if got != RedactedErrorSentinel {
		t.Errorf("expected fail-closed sentinel %q; got %q", RedactedErrorSentinel, got)
	}
	if items != nil {
		t.Errorf("expected nil items on error; got %v", items)
	}
}

func TestRedactor_FailsClosed_OnUnreachableServer(t *testing.T) {
	// Start a server and immediately close it to get a guaranteed-unreachable URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	r := NewRedactor(sidecar.NewPresidioClient(url, 500*time.Millisecond), true, nil, 0)
	got, _, err := r.RedactText(context.Background(), "secret text")
	if err == nil {
		t.Fatal("expected connection error")
	}
	if got != RedactedErrorSentinel {
		t.Errorf("expected fail-closed sentinel; got %q", got)
	}
}

func TestRedactor_CacheSkipsAnalyzer(t *testing.T) {
	var hits int32
	entities := map[string][]sidecar.PresidioEntity{
		"repeat me": {
			{Start: 0, End: 9, Score: 0.95, EntityType: "PERSON"},
		},
	}
	srv := fakePresidio(t, entities, &hits)
	defer srv.Close()

	r := NewRedactor(sidecar.NewPresidioClient(srv.URL, time.Second), true, nil, 0)
	ctx := context.Background()

	// First call → hits analyzer.
	got1, _, _ := r.RedactText(ctx, "repeat me")
	if got1 != "<PERSON>" {
		t.Errorf("first call: expected <PERSON>; got %q", got1)
	}

	// Second call with same text → should hit cache, not analyzer.
	got2, _, _ := r.RedactText(ctx, "repeat me")
	if got2 != "<PERSON>" {
		t.Errorf("cached call: expected <PERSON>; got %q", got2)
	}

	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected exactly 1 analyzer hit (second was cached); got %d", hits)
	}
}

func TestRedactor_ConcurrentSafety(t *testing.T) {
	// "concurrent test" — "concurrent" is chars 0..9 inclusive, so End=10.
	entities := map[string][]sidecar.PresidioEntity{
		"concurrent test": {
			{Start: 0, End: 10, Score: 0.95, EntityType: "PERSON"},
		},
	}
	srv := fakePresidio(t, entities, nil)
	defer srv.Close()

	r := NewRedactor(sidecar.NewPresidioClient(srv.URL, time.Second), true, nil, 0)
	ctx := context.Background()

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			got, _, err := r.RedactText(ctx, "concurrent test")
			if err != nil {
				t.Errorf("concurrent call errored: %v", err)
				return
			}
			if got != "<PERSON> test" {
				t.Errorf("concurrent call returned %q (want %q)", got, "<PERSON> test")
			}
		}()
	}
	wg.Wait()
}

func TestRedactor_EntitiesReturnsConfiguredList(t *testing.T) {
	r := NewRedactor(nil, false, []string{"PERSON", "EMAIL_ADDRESS"}, 0.5)
	got := r.Entities()
	if len(got) != 2 || got[0] != "PERSON" || got[1] != "EMAIL_ADDRESS" {
		t.Errorf("expected configured entities; got %v", got)
	}

	// Mutating the returned slice must not affect the redactor.
	got[0] = "MUTATED"
	again := r.Entities()
	if again[0] != "PERSON" {
		t.Errorf("entities slice was not defensive-copied; got %v", again)
	}
}

func TestRedactor_DefaultEntitiesUsedWhenNoneConfigured(t *testing.T) {
	r := NewRedactor(nil, false, nil, 0)
	got := r.Entities()
	if len(got) == 0 {
		t.Fatal("expected default entities when none configured")
	}
	// Spot-check that PERSON is in the default set.
	found := false
	for _, e := range got {
		if e == "PERSON" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected PERSON in default entities; got %v", got)
	}
}

func TestRedactor_NilRedactorSafe(t *testing.T) {
	var r *Redactor
	if r.Enabled() {
		t.Error("nil redactor should report Enabled()=false")
	}
	if r.Entities() != nil {
		t.Error("nil redactor should return nil entities")
	}
}
