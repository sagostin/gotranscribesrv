import Vapor

/// Health check route — reports engine status.
func healthRoutes(_ app: Application, engines: EngineManager) {
    app.get("health") { req async -> HealthResponse in
        let status = await engines.healthStatus()
        let snap = await engines.getTtsDefaults()
        return HealthResponse(
            status: "ok",
            models: status,
            config: TtsDefaults(
                synthesizeBackend: snap.synthesizeBackend,
                streamBackend: snap.streamBackend,
                realtimeEngine: snap.realtimeEngine
            )
        )
    }
}

struct HealthResponse: Content {
    let status: String
    let models: [String: String]
    /// Resolved server-side defaults — surfaced so operators can verify
    /// which backend / engine each endpoint resolves to when no
    /// explicit per-request override is supplied.
    let config: TtsDefaults
}

struct TtsDefaults: Content {
    let synthesizeBackend: String  // /synthesize default (SIDECAR_TTS_DEFAULT_BACKEND)
    let streamBackend: String      // /synthesize/stream default (SIDECAR_TTS_STREAM_BACKEND; always "pocket" today)
    let realtimeEngine: String     // /stream/realtime default (SIDECAR_REALTIME_ENGINE; default "eou-320")
}
