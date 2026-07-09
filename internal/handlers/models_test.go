package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func newModelsApp() *fiber.App {
	app := fiber.New()
	app.Get("/v1/models", ListModels)
	return app
}

func getModels(t *testing.T, app *fiber.App, path string) ModelList {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil), -1)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("GET %s → %d: %s", path, resp.StatusCode, body)
	}
	var list ModelList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, body)
	}
	return list
}

func TestListModels_ReturnsCanonicalShape(t *testing.T) {
	app := newModelsApp()
	list := getModels(t, app, "/v1/models")

	if list.Object != "list" {
		t.Errorf("envelope object = %q, want %q", list.Object, "list")
	}
	if len(list.Data) == 0 {
		t.Fatal("data array is empty")
	}
	for i, m := range list.Data {
		if m.Object != "model" {
			t.Errorf("data[%d].object = %q, want %q", i, m.Object, "model")
		}
		if m.ID == "" {
			t.Errorf("data[%d].id is empty", i)
		}
		if m.OwnedBy == "" {
			t.Errorf("data[%d].owned_by is empty", i)
		}
		if m.Created <= 0 {
			t.Errorf("data[%d].created = %d, want positive unix timestamp", i, m.Created)
		}
	}
}

func TestListModels_ContainsExpectedIDs(t *testing.T) {
	app := newModelsApp()
	list := getModels(t, app, "/v1/models")

	required := []string{
		"whisper-1",
		"gpt-4o-transcribe",
		"gpt-4o-transcribe-diarize",
		"parakeet-tdt-v3-coreml",
		"tts-1",
		"pocket-tts-1",
		"Meta-Llama-3.1-8B-Instruct-4bit",
	}

	have := make(map[string]bool, len(list.Data))
	for _, m := range list.Data {
		have[m.ID] = true
	}

	for _, id := range required {
		if !have[id] {
			t.Errorf("expected model %q in catalog, not found", id)
		}
	}
}

func TestListModels_OwnedByFilter(t *testing.T) {
	app := newModelsApp()
	list := getModels(t, app, "/v1/models?owned_by=openai")

	if len(list.Data) == 0 {
		t.Fatal("owned_by=openai filter returned empty list")
	}
	for _, m := range list.Data {
		if m.OwnedBy != "openai" {
			t.Errorf("owned_by=openai filter leaked %q (owner=%q)", m.ID, m.OwnedBy)
		}
	}
	for _, m := range list.Data {
		if m.ID == "parakeet-tdt-v3-coreml" {
			t.Errorf("parakeet should not appear under owned_by=openai")
		}
	}
}

func TestListModels_UnknownOwner(t *testing.T) {
	app := newModelsApp()
	list := getModels(t, app, "/v1/models?owned_by=nonexistent")

	if list.Object != "list" {
		t.Errorf("object = %q, want list", list.Object)
	}
	if len(list.Data) != 0 {
		t.Errorf("expected empty data, got %d entries", len(list.Data))
	}
}
