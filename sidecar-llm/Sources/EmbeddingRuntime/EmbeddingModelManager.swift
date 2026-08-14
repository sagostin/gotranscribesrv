import CoreML
import Foundation
@preconcurrency import Embeddings
import ModelRuntime

/// Type-erased embedding model (Bert / ModernBert / NomicBert bundles).
struct AnyEmbeddingModel: Sendable {
    /// Returns (embedding vector, prompt token count).
    let encode: @Sendable (String) async throws -> ([Float], Int)
    let dimensions: Int
}

/// Owns embedding models: download (via swift-embeddings' hub loader), residency, encoding.
/// Only instantiated when the embeddings feature is enabled.
public actor EmbeddingModelManager {
    private let registry: ModelRegistry
    private let settings: ServerSettings
    private let downloadBase: URL
    private let downloader: ModelDownloader

    private var models: [String: AnyEmbeddingModel] = [:]
    private var statuses: [String: ModelStatus] = [:]

    public init(registry: ModelRegistry, downloader: ModelDownloader) {
        self.registry = registry
        self.settings = registry.settings
        self.downloader = downloader
        self.downloadBase = downloader.downloadBase
        for entry in registry.entries(kind: .embedding) {
            statuses[entry.id] = .notDownloaded
        }
    }

    public func status(id: String) -> ModelStatus { statuses[id] ?? .notDownloaded }

    /// swift-embeddings downloads on load, so "download" and "load" coincide here.
    public func download(id: String) async throws {
        try await load(id: id)
    }

    public func load(id: String) async throws {
        _ = try await model(for: id)
    }

    public func unload(id: String) {
        models[id] = nil
        if statuses[id] == .ready { statuses[id] = .downloaded }
    }

    public func dimensions(id: String) -> Int? {
        models[id]?.dimensions
    }

    public func embed(id: String, inputs: [String]) async throws -> (vectors: [[Float]], promptTokens: Int) {
        let model = try await model(for: id)
        var vectors: [[Float]] = []
        var tokens = 0
        for input in inputs {
            let (vector, count) = try await model.encode(input)
            vectors.append(vector)
            tokens += count
        }
        return (vectors, tokens)
    }

    // MARK: - Internals

    private func entry(for id: String) throws -> ModelRegistryEntry {
        guard let entry = registry.models.first(where: { $0.id == id }) else {
            throw ModelError.unknownModel(id)
        }
        guard entry.kind == .embedding else {
            throw ModelError.wrongKind(id, kind: entry.kind.rawValue)
        }
        return entry
    }

    private func model(for id: String) async throws -> AnyEmbeddingModel {
        if let model = models[id] { return model }
        let entry = try entry(for: id)

        let repoFolder = downloader.localRepoURL(entry.repo)
        if !FileManager.default.fileExists(atPath: repoFolder.path) && !settings.autoDownload {
            statuses[id] = .failed(ModelError.autoDownloadDisabled(id).description)
            throw ModelError.autoDownloadDisabled(id)
        }
        statuses[id] = .loading
        do {
            let model = try await Self.load(entry: entry, downloadBase: downloadBase)
            models[id] = model
            statuses[id] = .ready
            return model
        } catch {
            statuses[id] = .failed(error.localizedDescription)
            throw error
        }
    }

    private static func load(entry: ModelRegistryEntry, downloadBase: URL) async throws -> AnyEmbeddingModel {
        let encode: @Sendable (String) async throws -> ([Float], Int)
        switch (entry.architecture ?? "bert").lowercased() {
        case "modernbert":
            let bundle = try await ModernBert.loadModelBundle(
                from: entry.repo, downloadBase: downloadBase)
            encode = { text in
                let tensor = try bundle.encode(text)
                let tokens = try bundle.tokenizer.tokenizeText(text).count
                return (await tensor.shapedArray(of: Float32.self).scalars, tokens)
            }
        case "nomicbert":
            let bundle = try await NomicBert.loadModelBundle(
                from: entry.repo, downloadBase: downloadBase)
            encode = { text in
                let tensor = try bundle.encode(text)
                let tokens = try bundle.tokenizer.tokenizeText(text).count
                return (await tensor.shapedArray(of: Float32.self).scalars, tokens)
            }
        default: // bert
            let bundle = try await Bert.loadModelBundle(
                from: entry.repo, downloadBase: downloadBase)
            encode = { text in
                let tensor = try bundle.encode(text)
                let tokens = try bundle.tokenizer.tokenizeText(text).count
                return (await tensor.shapedArray(of: Float32.self).scalars, tokens)
            }
        }
        // Probe encode determines the dimensionality and warms the model.
        let (probe, _) = try await encode("warmup")
        return AnyEmbeddingModel(encode: encode, dimensions: probe.count)
    }
}
