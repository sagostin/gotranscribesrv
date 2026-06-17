package handlers

import (
	"log/slog"

	"github.com/gofiber/fiber/v2"
	"github.com/shaunagostinho/gotranscribesrv/internal/logging"
	"github.com/shaunagostinho/gotranscribesrv/internal/metrics"
	"github.com/shaunagostinho/gotranscribesrv/internal/middleware"
	"github.com/shaunagostinho/gotranscribesrv/internal/sidecar"
)

// ProcessHandler handles LLM transcript processing routes.
type ProcessHandler struct {
	sidecar *sidecar.Client
	lm      *logging.LogManager
}

// NewProcessHandler creates a new ProcessHandler.
func NewProcessHandler(sc *sidecar.Client, lm *logging.LogManager) *ProcessHandler {
	return &ProcessHandler{sidecar: sc, lm: lm}
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

	h.lm.SendLog(h.lm.BuildLog("LLM_PROCESS_STARTED", "LLMProcessStarted", slog.LevelInfo, map[string]interface{}{
		"endpoint":     "/api/v1/process",
		"task":         req.Task,
		"input_length": len(req.TranscriptText),
		"max_tokens":   req.MaxTokens,
		"temperature":  req.Temperature,
		"language":     req.Language,
		"request_id":   middleware.RequestIDFromCtx(c),
	}))

	result, err := h.sidecar.Process(req)
	if err != nil {
		slog.ErrorContext(c.UserContext(), "LLM processing failed", "error", err, "task", req.Task)
		h.lm.SendLog(h.lm.BuildLog("LLM_PROCESS_FAILED", "LLMProcessFailed", slog.LevelError, map[string]interface{}{
			"endpoint":     "/api/v1/process",
			"task":         req.Task,
			"input_length": len(req.TranscriptText),
			"request_id":   middleware.RequestIDFromCtx(c),
		}, err))
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": fiber.Map{
				"code":    "SIDECAR_ERROR",
				"message": "LLM processing service unavailable",
				"status":  502,
			},
		})
	}

	// Record LLM metrics
	metrics.RecordLLMUsage(result.Task, result.TokensGenerated, result.ProcessTimeMs)

	h.lm.SendLog(h.lm.BuildLog("LLM_PROCESS_COMPLETED", "LLMProcessCompleted", slog.LevelInfo, map[string]interface{}{
		"endpoint":         "/api/v1/process",
		"task":             result.Task,
		"model":            result.Model,
		"input_length":     len(req.TranscriptText),
		"output_length":    len(result.Result),
		"tokens_generated": result.TokensGenerated,
		"process_time_ms":  result.ProcessTimeMs,
		"result":           result.Result,
		"request_id":       middleware.RequestIDFromCtx(c),
	}))

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
		slog.ErrorContext(c.UserContext(), "LLM tasks list failed", "error", err)
		h.lm.SendLog(h.lm.BuildLog("LLM_TASKS_LIST_FAILED", "LLMTasksListFailed", slog.LevelError, map[string]interface{}{
			"endpoint":   "/api/v1/process/tasks",
			"request_id": middleware.RequestIDFromCtx(c),
		}, err))
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
