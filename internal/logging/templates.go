// Package logging provides a non-blocking, templated logging facility that
// optionally ships structured events to Grafana Loki. The shape of the
// public API (LogManager, Templates, BuildLog, SendLog, LoggingFormat)
// mirrors the gomsggw project's log.go for stylistic parity, but the
// local-emit backend is stdlib log/slog (matching the rest of this
// codebase) and the LokiClient is a verbatim port of the push-API client.
package logging

// LoadTemplates seeds the LogManager.Templates map with every named
// event used throughout the service. Handlers call BuildLog with one
// of these names + args, and the manager formats the message via
// fmt.Sprintf at SendLog time. Adding a new event: add it here.
func (lm *LogManager) LoadTemplates() {
	templates := map[string]string{
		// ── ASR (file) ──────────────────────────────────────────
		"ASRRequestReceived": "ASR request received",
		"ASRFileTooLarge":    "ASR file exceeds 100MB limit",
		"ASRMissingAudio":    "ASR request missing 'audio' file",
		"ASRFileReadError":   "ASR failed to read uploaded file: %v",
		"ASRCompleted":       "ASR transcription completed",
		"ASRFailed":          "ASR transcription failed: %v",
		"ASRSidecarError":    "ASR sidecar returned non-200: %v",

		// ── Whisper-compatible ASR ──────────────────────────────
		"WhisperRequestReceived": "Whisper-compat request received",
		"WhisperCompleted":       "Whisper-compat transcription completed",
		"WhisperFailed":          "Whisper-compat transcription failed: %v",

		// ── Deepgram-compatible streaming ASR ───────────────────
		"DeepgramSessionStarted": "Deepgram-compat session started",
		"DeepgramSessionEnded":   "Deepgram-compat session ended",
		"DeepgramSessionError":   "Deepgram-compat session error: %v",
		"DeepgramConnectFailed":  "Deepgram-compat sidecar connect failed: %v",

		// ── Watson-compatible ASR ───────────────────────────────
		"WatsonRecognizeReceived":  "Watson-compat POST /v1/recognize received",
		"WatsonRecognizeFailed":    "Watson transcription failed: %v",
		"WatsonRecognizeCompleted": "Watson transcription completed",
		"WatsonSessionStarted":     "Watson-compat session started",
		"WatsonSessionEnded":       "Watson-compat session ended",
		"WatsonSidecarError":       "Watson sidecar error: %v",
		"WatsonConnectFailed":      "Watson-compat sidecar connect failed: %v",
		"WatsonClientReadError":    "Watson client read error: %v",
		"WatsonForwardFailed":      "Watson failed to forward audio to sidecar: %v",

		// ── WebSocket ASR (native) ──────────────────────────────
		"WSASRSessionStarted": "WebSocket ASR session started",
		"WSASRSessionEnded":   "WebSocket ASR session ended",
		"WSASRConnectFailed":  "WebSocket ASR sidecar connect failed: %v",

		// ── TTS ─────────────────────────────────────────────────
		"TTSRequestReceived": "TTS request received",
		"TTSCompleted":       "TTS synthesis completed",
		"TTSFailed":          "TTS synthesis failed: %v",
		"TTSVoiceLoadFailed": "TTS failed to load stored voice: %v",
		"TTSSidecarError":    "TTS sidecar returned non-200: %v",

		// ── Voice cloning ───────────────────────────────────────
		"VoiceCloneStarted":    "Voice clone started",
		"VoiceCloneCompleted":  "Voice clone completed",
		"VoiceCloneFailed":     "Voice cloning failed: %v",
		"VoiceCloneDirError":   "Voice clone directory error: %v",
		"VoiceCloneWriteError": "Voice clone failed to write embedding: %v",
		"VoiceCloneDBError":    "Voice clone DB record error: %v",

		// ── LLM processing ──────────────────────────────────────
		"LLMProcessStarted":   "LLM processing started",
		"LLMProcessCompleted": "LLM processing completed",
		"LLMProcessFailed":    "LLM processing failed: %v",
		"LLMTasksListFailed":  "LLM tasks list failed: %v",

		// ── Sidecar client (transport-level) ────────────────────
		"SidecarCallStarted": "Sidecar call started",
		"SidecarCallOK":      "Sidecar call completed",
		"SidecarCallFailed":  "Sidecar call failed: %v",
		"SidecarDecodeError": "Sidecar response decode failed: %v",

		// ── Auth / HTTP middleware failures ─────────────────────
		"AggregatedRequestFailed": "Request failed",

		// ── Loki shipper (self) ─────────────────────────────────
		"LokiPushFailed":  "Loki push failed: %v",
		"LokiChannelFull": "Loki log channel full, dropping log",

		// ── Catch-all ───────────────────────────────────────────
		"GenericError":       "An error occurred: %v",
		"UnexpectedError":    "Unexpected error: %v",
		"UnhandledException": "Unhandled exception: %v",
	}

	for name, template := range templates {
		lm.AddTemplate(name, template)
	}
}
