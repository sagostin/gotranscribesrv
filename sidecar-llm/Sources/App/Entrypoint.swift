import Foundation
import Logging
import EmbeddingRuntime
import ExternalRuntime
import ImageRuntime
import ModelRuntime
import Tooling
import Vapor

@main
enum Entrypoint {
    static func main() async throws {
        let base = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let modelsDir = base.appending(path: "Models")
        let registryURL = base.appending(path: "models.json")

        var registry: ModelRegistry
        do {
            registry = try ModelRegistry.load(from: registryURL)
            print("[server] loaded \(registry.models.count) model entries from models.json")
        } catch {
            print("[server] WARNING: could not load models.json (\(error)); starting with an empty registry")
            registry = ModelRegistry(models: [])
        }

        // Env vars override models.json settings.
        let settings = registry.settings.applyingEnvironment()
        registry = ModelRegistry(models: registry.models, settings: settings)
        print("[server] settings: autoDownload=\(settings.autoDownload) preload=\(settings.preload) images=\(settings.features.images) embeddings=\(settings.features.embeddings)")

        let downloader = ModelDownloader(
            downloadBase: modelsDir.appending(path: "hf"),
            hfToken: ProcessInfo.processInfo.environment["HUGGING_FACE_HUB_TOKEN"]
        )
        let manager = ModelManager(
            registry: registry,
            downloader: downloader,
            compiledDir: modelsDir.appending(path: "compiled"),
            maxResident: Int(ProcessInfo.processInfo.environment["COREML_MAX_RESIDENT"] ?? "") ?? 2
        )
        let images = settings.features.images
            ? ImageModelManager(registry: registry, downloader: downloader)
            : nil
        let embeddings = settings.features.embeddings
            ? EmbeddingModelManager(registry: registry, downloader: downloader)
            : nil
        let coremlLLM = CoreMLLLMManager(settings: settings)
        await coremlLLM.setEntries(
            registry.models.filter { $0.runtime == .coremlLLM && $0.kind == .chat })

        var environment = try Environment.detect()
        try LoggingSystem.bootstrap(from: &environment)
        let app = try await Application.make(environment)
        if let port = Int(ProcessInfo.processInfo.environment["PORT"] ?? "") {
            app.http.server.configuration.port = port
        }
        // Bind all interfaces by default so Docker (host.docker.internal) can
        // reach the sidecar — same default as the audio sidecar's
        // AUDIO_SIDECAR_HOST. Set LLM_SIDECAR_HOST=127.0.0.1 to restrict to
        // loopback.
        app.http.server.configuration.hostname =
            ProcessInfo.processInfo.environment["LLM_SIDECAR_HOST"] ?? "0.0.0.0"

        try routes(app, context: ServerContext(
            settings: settings, manager: manager, images: images,
            embeddings: embeddings, coremlLLM: coremlLLM))

        // Preload in the background so the HTTP port opens immediately.
        Task {
            await manager.preload()
        }

        try await app.execute()
        try await app.asyncShutdown()
    }
}
