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
		// Transcript fields on *_SENT events are PII-redacted before
		// the event is built — never log raw sidecar text.
		"DeepgramPartialSent":         "Deepgram-compat partial result sent",
		"DeepgramFinalSent":           "Deepgram-compat final result sent",
		"DeepgramRealtimePartialSent": "Deepgram-realtime partial result sent",
		"DeepgramRealtimeFinalSent":   "Deepgram-realtime final result sent",

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

		// ── OpenAI-compatible TTS ───────────────────────────────
		"OpenAITTSRequest":         "OpenAI-compat TTS request received",
		"OpenAITTSCompleted":       "OpenAI-compat TTS synthesis completed",
		"OpenAITTSFailed":          "OpenAI-compat TTS synthesis failed: %v",
		"OpenAITTSTranscodeFailed": "OpenAI-compat TTS audio transcode failed: %v",

		// ── LLM gateway (OpenAI + Anthropic dialects) ───────────
		// Content (prompts/completions) is NEVER logged — metadata
		// only: model, sizes, token counts, timings, tool names.
		"LLMRequestReceived":     "LLM request received",
		"LLMRequestCompleted":    "LLM request completed",
		"LLMStreamStarted":       "LLM stream started",
		"LLMStreamProgress":      "LLM stream in progress",
		"LLMStreamCompleted":     "LLM stream completed",
		"LLMStreamAborted":       "LLM stream aborted: %v",
		"LLMUpstreamRejected":    "LLM sidecar rejected request: %v",
		"LLMSidecarUnavailable":  "LLM sidecar unavailable: %v",
		"ResponsesPersistFailed": "Responses state persist failed: %v",

		// ── Conversations API (Responses state) ─────────────────
		"ConversationCreated": "Conversation created",
		"ConversationDBError": "Conversation DB error: %v",

		// ── Voice cloning ───────────────────────────────────────
		"VoiceCloneStarted":    "Voice clone started",
		"VoiceCloneCompleted":  "Voice clone completed",
		"VoiceCloneFailed":     "Voice cloning failed: %v",
		"VoiceCloneDirError":   "Voice clone directory error: %v",
		"VoiceCloneWriteError": "Voice clone failed to write embedding: %v",
		"VoiceCloneDBError":    "Voice clone DB record error: %v",

		// ── PII redaction ─────────────────────────────────────────
		// Emitted when the Presidio analyzer is unreachable, errors,
		// or returns an invalid response. The associated log field
		// will contain the literal "<REDACTED-ERROR>" sentinel. The
		// presence of this event in Loki indicates PII may not have
		// been scrubbed from the related *_COMPLETED log entry.
		"PIIRedactorError": "PII redactor error: %v",

		// ── Auth failures ─────────────────────────────────────────
		// SECURITY: the raw token, API key, or password is NEVER
		// included in this event — only the auth method, reason, IP,
		// user agent, and request id. Operators see failed-auth
		// patterns in Loki via {type="AUTH_FAILED"}.
		"AuthFailed": "Authentication failed: method=%s reason=%s",

		// ── Voice cloning failures (5xx storage/DB paths) ──────────
		// Reached via the VOICE_CLONE_FAILED template already; the
		// following cover the storage / DB error paths that fire
		// AFTER a successful sidecar clone.
		"VoiceListDBError":   "Voice list DB query failed: %v",
		"VoiceDeleteDBError": "Voice delete DB error: %v",

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
