import CoreML
import Foundation
@preconcurrency import Models
@preconcurrency import Tokenizers
@preconcurrency import Hub

/// Lifecycle state of a registry model.
public enum ModelStatus: Sendable, Equatable {
    case notDownloaded
    case downloading(progress: Double)
    case downloaded
    case compiling
    case loading
    case ready
    case failed(String)

    public var label: String {
        switch self {
        case .notDownloaded: return "not_downloaded"
        case .downloading(let p): return "downloading(\(Int(p * 100))%)"
        case .downloaded: return "downloaded"
        case .compiling: return "compiling"
        case .loading: return "loading"
        case .ready: return "ready"
        case .failed(let msg): return "failed: \(msg)"
        }
    }
}

/// Detected model interface family.
public enum ModelInterface: String, Sendable {
    /// inputIds (+masks) -> logits (apple/mistral-coreml convention).
    case logits
    /// inputIds (+causalMask) -> top-k ids + scores (groxaxo qwen3 family).
    case topK
}

/// Owns downloads, compiled-model caching, and the set of resident chat models (LRU).
public actor ModelManager {
    private let registry: ModelRegistry
    public let settings: ServerSettings
    private let downloader: ModelDownloader
    private let compiledDir: URL
    private let maxResident: Int

    private var statuses: [String: ModelStatus] = [:]
    private var runners: [String: ModelRunner] = [:]
    private var lastUsed: [String: Date] = [:]

    public init(
        registry: ModelRegistry,
        downloader: ModelDownloader,
        compiledDir: URL,
        maxResident: Int = 2
    ) {
        self.registry = registry
        self.settings = registry.settings
        self.downloader = downloader
        self.compiledDir = compiledDir
        self.maxResident = max(1, maxResident)
        for entry in registry.models {
            statuses[entry.id] = .notDownloaded
        }
    }

    public var entries: [ModelRegistryEntry] { registry.models }

    public func entries(kind: ModelKind) -> [ModelRegistryEntry] {
        registry.entries(kind: kind)
    }

    public func status(id: String) -> ModelStatus {
        statuses[id] ?? .notDownloaded
    }

    public func allStatuses() -> [String: String] {
        statuses.mapValues { $0.label }
    }

    /// Download the model + tokenizer files without loading. Explicit user action;
    /// works even when autoDownload is disabled.
    public func download(id: String) async throws {
        let entry = try entry(for: id)
        if case .downloading = statuses[id] { return }
        do {
            try await downloadFiles(entry)
            if statuses[id] == nil || statuses[id] == .notDownloaded {
                statuses[id] = .downloaded
            }
            if case .downloading = statuses[id] { statuses[id] = .downloaded }
        } catch {
            statuses[id] = .failed(error.localizedDescription)
            throw error
        }
    }

    /// Download (if needed and permitted), compile-cache, load, warm up, and return the runner.
    public func runner(for id: String) async throws -> ModelRunner {
        if let runner = runners[id] {
            lastUsed[id] = Date()
            return runner
        }
        let entry = try entry(for: id)
        guard entry.kind == .chat else {
            throw ModelError.wrongKind(id, kind: entry.kind.rawValue)
        }
        if entry.runtime == .coremlLLM {
            throw ModelError.unsupportedRuntime(id, runtime: entry.runtime.rawValue)
        }
        do {
            try await downloadFiles(entry, implicit: true)

            statuses[id] = .compiling
            let packageURL = try downloader.findPackage(
                in: downloader.localRepoURL(entry.repo), entry: entry)
            let compiledURL = try await compiledModelURL(for: packageURL, id: id)

            statuses[id] = .loading
            let interface = try Self.detectInterface(modelURL: compiledURL, id: id)

            // Tokenizer + config first (top-k models need vocab_size from config.json).
            let tokenizerFolder = try await downloader.downloadTokenizerFiles(for: entry)
            let hubConfig = LanguageModelConfigurationFromHub(
                modelFolder: tokenizerFolder, hubApi: downloader.hub)
            guard let tokenizerConfig = try await hubConfig.tokenizerConfig else {
                throw ModelError.downloadFailed("tokenizer_config.json missing for \(id)")
            }
            let tokenizerData = try await hubConfig.tokenizerData
            let tokenizer = try AutoTokenizer.from(
                tokenizerConfig: tokenizerConfig, tokenizerData: tokenizerData)

            let model: any ChatModel
            switch interface {
            case .logits:
                model = try LanguageModel.loadCompiled(url: compiledURL, computeUnits: .all)
            case .topK:
                guard let modelConfig = try await hubConfig.modelConfig,
                      let vocabSize = modelConfig.vocabSize?.integer()
                else {
                    throw ModelError.incompatibleLayout(id, inputs: "config.json has no vocab_size")
                }
                let config = MLModelConfiguration()
                config.computeUnits = .all
                model = try TopKLanguageModel(
                    model: MLModel(contentsOf: compiledURL, configuration: config),
                    vocabSize: vocabSize)
            }

            let runner = ModelRunner(
                id: id,
                context: ModelContext(model: model, tokenizer: tokenizer, entry: entry))
            runners[id] = runner
            lastUsed[id] = Date()
            evictIfNeeded(except: id)

            await runner.warmup()
            statuses[id] = .ready
            return runner
        } catch {
            statuses[id] = .failed(error.localizedDescription)
            runners[id] = nil
            throw error
        }
    }

    public func unload(id: String) {
        runners[id] = nil
        lastUsed[id] = nil
        if statuses[id] == .ready { statuses[id] = .downloaded }
    }

    /// Load every chat entry marked `preload`, sequentially. Errors are logged, not thrown.
    public func preload() async {
        guard settings.preload else {
            print("[ModelManager] preloading disabled by settings")
            return
        }
        for entry in registry.entries(kind: .chat) where entry.preload {
            do {
                print("[ModelManager] preloading \(entry.id)...")
                _ = try await runner(for: entry.id)
                print("[ModelManager] \(entry.id) ready")
            } catch {
                print("[ModelManager] preload failed for \(entry.id): \(error.localizedDescription)")
            }
        }
    }

    // MARK: - Internals

    private func entry(for id: String) throws -> ModelRegistryEntry {
        guard let entry = registry.models.first(where: { $0.id == id }) else {
            throw ModelError.unknownModel(id)
        }
        return entry
    }

    private func downloadFiles(_ entry: ModelRegistryEntry, implicit: Bool = false) async throws {
        // Skip the (slow) model glob check if the package already exists locally.
        let packagePresent = (try? downloader.findPackage(
            in: downloader.localRepoURL(entry.repo), entry: entry)) != nil
        if !packagePresent {
            if implicit && !settings.autoDownload {
                throw ModelError.autoDownloadDisabled(entry.id)
            }
            statuses[entry.id] = .downloading(progress: 0)
            let id = entry.id
            try await downloader.downloadModelFiles(for: entry) { fraction in
                Task { await self.setDownloadProgress(id: id, fraction: fraction) }
            }
            // Verify the globs actually matched a model package.
            _ = try downloader.findPackage(
                in: downloader.localRepoURL(entry.repo), entry: entry)
        }
        statuses[entry.id] = .downloaded
    }

    private func setDownloadProgress(id: String, fraction: Double) {
        if case .downloading = statuses[id] {
            statuses[id] = .downloading(progress: fraction)
        }
    }

    /// Returns a ready-to-load compiled model (.mlmodelc), compiling and caching
    /// on first use so subsequent server starts load in seconds.
    private func compiledModelURL(for packageURL: URL, id: String) async throws -> URL {
        let fm = FileManager.default
        try fm.createDirectory(at: compiledDir, withIntermediateDirectories: true)

        if packageURL.pathExtension == "mlmodelc" {
            return packageURL // already compiled; load directly
        }

        let destination = compiledDir.appending(path: "\(id).mlmodelc")
        if fm.fileExists(atPath: destination.path) {
            return destination
        }

        print("[ModelManager] compiling \(id) (one-time, can take a while)...")
        let compiled = try await Task.detached {
            try MLModel.compileModel(at: packageURL)
        }.value
        if fm.fileExists(atPath: destination.path) {
            try fm.removeItem(at: destination)
        }
        try fm.moveItem(at: compiled, to: destination)
        print("[ModelManager] compiled \(id) -> \(destination.path)")
        return destination
    }

    /// Probe the model interface (CPU-only load; no ANE compile) and classify it.
    private static func detectInterface(modelURL: URL, id: String) throws -> ModelInterface {
        let config = MLModelConfiguration()
        config.computeUnits = .cpuOnly
        let probe = try MLModel(contentsOf: modelURL, configuration: config)
        let description = probe.modelDescription

        guard let input = description.inputDescriptionsByName["inputIds"],
              input.multiArrayConstraint?.shapeConstraint != nil
        else {
            let inputs = description.inputDescriptionsByName.keys.sorted().joined(separator: ", ")
            throw ModelError.incompatibleLayout(id, inputs: inputs)
        }

        if description.outputDescriptionsByName["logits"] != nil {
            return .logits
        }
        // Top-k family: exactly two outputs, one int (indices) + one float (scores).
        let outputs = description.outputDescriptionsByName
        let hasInt = outputs.values.contains { $0.multiArrayConstraint?.dataType == .int32 }
        let hasFloat = outputs.values.contains {
            [.float16, .float32, .double].contains($0.multiArrayConstraint?.dataType)
        }
        if outputs.count == 2 && hasInt && hasFloat {
            return .topK
        }
        let names = outputs.keys.sorted().joined(separator: ", ")
        throw ModelError.incompatibleLayout(id, inputs: "unrecognized outputs: \(names)")
    }

    private func evictIfNeeded(except keepID: String) {
        while runners.count > maxResident,
              let victim = lastUsed
                .filter({ $0.key != keepID })
                .min(by: { $0.value < $1.value })?.key
        {
            print("[ModelManager] evicting \(victim) (LRU)")
            unload(id: victim)
        }
    }
}
