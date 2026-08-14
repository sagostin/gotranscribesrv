package handlers

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// Model is the OpenAI /v1/models list entry schema.
// https://platform.openai.com/docs/api-reference/models/object
// Extra LLM-sidecar fields (kind, runtime, status, ...) are passed through.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	// LLM sidecar extras (absent for static STT/TTS catalog entries)
	Kind    string `json:"kind,omitempty"`    // chat | image | embedding
	Runtime string `json:"runtime,omitempty"` // standard | coreml-llm
	Status  string `json:"status,omitempty"`
	Repo    string `json:"repo,omitempty"`
}

// ModelList is the OpenAI /v1/models response envelope.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// ModelsHandler serves the merged GET /v1/models list: the static STT/TTS
// catalog plus the live LLM sidecar registry.
type ModelsHandler struct {
	sc *sidecar.Client
}

// NewModelsHandler constructs the handler. sc may be used to reach the
// LLM sidecar; when the sidecar is unreachable the list degrades to the
// static audio catalog.
func NewModelsHandler(sc *sidecar.Client) *ModelsHandler {
	return &ModelsHandler{sc: sc}
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
	}
}

// List handles GET /v1/models — OpenAI-compatible model listing.
//
// Auth: required (registered on the authed group, alongside /v1/audio/*).
//
// The response merges the static STT/TTS catalog with the live LLM sidecar
// registry (chat / embedding / image entries with kind, runtime, and status).
//
// Query: ?owned_by=<string> filters results to entries matching the owner.
// Mirrors a slice of the OpenAI /v1/models API surface; pagination is
// omitted because the catalog is small.
func (h *ModelsHandler) List(c *fiber.Ctx) error {
	all := supportedModels()

	// Merge live LLM registry entries (chat, embeddings, image models)
	if llm := h.sc.LLMModels(); llm != nil {
		for _, m := range llm.Data {
			all = append(all, Model{
				ID:      m.ID,
				Object:  "model",
				Created: m.Created,
				OwnedBy: m.OwnedBy,
				Kind:    m.Kind,
				Runtime: m.Runtime,
				Status:  m.Status,
				Repo:    m.Repo,
			})
		}
	}

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
