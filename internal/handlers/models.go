package handlers

import (
	"strings"

	"github.com/gofiber/fiber/v2"
)

// Model is the OpenAI /v1/models list entry schema.
// https://platform.openai.com/docs/api-reference/models/object
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList is the OpenAI /v1/models response envelope.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Static catalog of models advertised via the OpenAI-compatible /v1/models
// endpoint. Includes both OpenAI-branded mock IDs (so unmodified OpenAI SDKs
// find a known model) and the real on-device models the server actually runs.
//
// The `model` field on /v1/audio/transcriptions is still not validated —
// unknown IDs are accepted and silently fall through to the real engine.
func supportedModels() []Model {
	return []Model{
		// ── STT — OpenAI mock IDs (for client SDK compatibility) ──────
		{ID: "whisper-1", Object: "model", Created: 1677649200, OwnedBy: "openai"},
		{ID: "gpt-4o-transcribe", Object: "model", Created: 1742000000, OwnedBy: "openai"},
		{ID: "gpt-4o-mini-transcribe", Object: "model", Created: 1742000000, OwnedBy: "openai"},
		{ID: "gpt-4o-transcribe-diarize", Object: "model", Created: 1742000000, OwnedBy: "openai"},

		// ── STT — real on-device model ──────────────────────────────
		{ID: "parakeet-tdt-v3-coreml", Object: "model", Created: 1735689600, OwnedBy: "nvidia"},

		// ── TTS — OpenAI mock IDs ────────────────────────────────────
		{ID: "tts-1", Object: "model", Created: 1696280400, OwnedBy: "openai"},
		{ID: "tts-1-hd", Object: "model", Created: 1696280400, OwnedBy: "openai"},
		{ID: "gpt-4o-mini-tts", Object: "model", Created: 1736380800, OwnedBy: "openai"},

		// ── TTS — real on-device model ──────────────────────────────
		{ID: "pocket-tts-1", Object: "model", Created: 1735603200, OwnedBy: "kyutai"},

		// ── LLM — real on-device model (Python sidecar / MLX) ───────
		{ID: "Meta-Llama-3.1-8B-Instruct-4bit", Object: "model", Created: 1725148800, OwnedBy: "meta"},
	}
}

// ListModels handles GET /v1/models — OpenAI-compatible model listing.
//
// Auth: required (registered on the authed group, alongside /v1/audio/*).
//
// Query: ?owned_by=<string> filters results to entries matching the owner.
// Mirrors a slice of the OpenAI /v1/models API surface; pagination is
// omitted because the catalog is static and small.
func ListModels(c *fiber.Ctx) error {
	all := supportedModels()

	ownedBy := strings.TrimSpace(c.Query("owned_by"))
	if ownedBy == "" {
		return c.JSON(ModelList{Object: "list", Data: all})
	}

	filtered := all[:0:0]
	for _, m := range all {
		if m.OwnedBy == ownedBy {
			filtered = append(filtered, m)
		}
	}
	return c.JSON(ModelList{Object: "list", Data: filtered})
}
