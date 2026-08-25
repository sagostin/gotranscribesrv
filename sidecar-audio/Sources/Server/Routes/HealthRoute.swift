import Foundation
import Vapor

/// Health check route — reports engine status.
func healthRoutes(_ app: Application, engines: EngineManager, build: BuildInfo?) {
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
            ),
            build: build
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
    /// Build marker written by `make audio-build` (git SHA + build time).
    /// nil when the marker file is absent (e.g. plain `swift build`).
    let build: BuildInfo?
}

/// BuildInfo is written next to the release binary by `make audio-build`
/// and read at startup — makes "which version is actually running on this
/// node" remotely verifiable via /health.
struct BuildInfo: Content {
    let sha: String
    let builtAt: String

    enum CodingKeys: String, CodingKey {
        case sha
        case builtAt = "built_at"
    }

    /// Load build-info.json from beside the executable (release layout:
    /// .build/release/Server + .build/release/build-info.json), falling
    /// back to the working directory.
    static func load() -> BuildInfo? {
        var candidates: [URL] = []
        let exeURL = URL(fileURLWithPath: CommandLine.arguments[0]).resolvingSymlinksInPath()
        candidates.append(exeURL.deletingLastPathComponent().appendingPathComponent("build-info.json"))
        candidates.append(URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
            .appendingPathComponent(".build/release/build-info.json"))
        for url in candidates {
            guard let data = try? Data(contentsOf: url),
                  let info = try? JSONDecoder().decode(BuildInfo.self, from: data) else { continue }
            return info
        }
        return nil
    }
}

struct TtsDefaults: Content {
    let synthesizeBackend: String  // /synthesize default (SIDECAR_TTS_DEFAULT_BACKEND)
    let streamBackend: String      // /synthesize/stream default (SIDECAR_TTS_STREAM_BACKEND; always "pocket" today)
    let realtimeEngine: String     // /stream/realtime default (SIDECAR_REALTIME_ENGINE; default "eou-320")
}
