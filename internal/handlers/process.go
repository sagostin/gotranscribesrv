package handlers

import (
	"log/slog"

	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// ProcessHandler handles LLM transcript processing routes.
type ProcessHandler struct {
	sidecar *sidecar.Client
}

// NewProcessHandler creates a new ProcessHandler.
func NewProcessHandler(sc *sidecar.Client) *ProcessHandler {
	return &ProcessHandler{sidecar: sc}
}

// Process runs LLM processing on transcript text.
// POST /api/v1/process
func (h *ProcessHandler) Process(c *fiber.Ctx) error {
	var req sidecar.ProcessRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "INVALID_INPUT",
				"message": "Invalid request body",
				"status":  422,
			},
		})
	}

	if req.TranscriptText == "" {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "MISSING_TEXT",
				"message": "transcript_text field is required",
				"status":  422,
			},
		})
	}

	// Default task
	if req.Task == "" {
		req.Task = "summarize"
	}

	// Validate task-specific requirements
	if req.Task == "translate" && req.Language == "" {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "MISSING_LANGUAGE",
				"message": "'language' field is required for translate task",
				"status":  422,
			},
		})
	}
	if (req.Task == "qa" || req.Task == "custom") && req.Prompt == "" {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "MISSING_PROMPT",
				"message": "'prompt' field is required for " + req.Task + " task",
				"status":  422,
			},
		})
	}

	// Defaults
	if req.MaxTokens == 0 {
		req.MaxTokens = 1024
	}
	if req.Temperature == 0 {
		req.Temperature = 0.3
	}

	result, err := h.sidecar.Process(req)
	if err != nil {
		slog.Error("LLM processing failed", "error", err, "task", req.Task)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "SIDECAR_ERROR",
				"message": "LLM processing service unavailable",
				"status":  502,
			},
		})
	}
	c.Locals("usage_meta", map[string]interface{}{
		"input_length":     len(req.TranscriptText),
		"task":             result.Task,
		"tokens_generated": result.TokensGenerated,
		"model":            result.Model,
	})

	return c.JSON(result)
}

// ListTasks returns available LLM processing tasks.
// GET /api/v1/process/tasks
func (h *ProcessHandler) ListTasks(c *fiber.Ctx) error {
	tasks, err := h.sidecar.ListTasks()
	if err != nil {
		slog.Error("LLM tasks list failed", "error", err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "SIDECAR_ERROR",
				"message": "LLM processing service unavailable",
				"status":  502,
			},
		})
	}

	return c.JSON(tasks)
}
