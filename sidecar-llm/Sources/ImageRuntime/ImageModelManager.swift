import AppKit
import CoreML
import Foundation
import ModelRuntime
@preconcurrency import StableDiffusion

public struct GeneratedImage: Sendable {
    public var pngData: Data
    public var seed: UInt32
}

/// Owns Stable Diffusion pipelines: download, residency, generation.
/// Only instantiated when the images feature is enabled.
public actor ImageModelManager {
    private let registry: ModelRegistry
    private let settings: ServerSettings
    private let downloader: ModelDownloader

    private var pipelines: [String: StableDiffusionPipeline] = [:]
    private var statuses: [String: ModelStatus] = [:]

    public init(registry: ModelRegistry, downloader: ModelDownloader) {
        self.registry = registry
        self.settings = registry.settings
        self.downloader = downloader
        for entry in registry.entries(kind: .image) {
            statuses[entry.id] = .notDownloaded
        }
    }

    public func status(id: String) -> ModelStatus { statuses[id] ?? .notDownloaded }

    /// Explicit download (works even when autoDownload is off).
    public func download(id: String) async throws {
        let entry = try entry(for: id)
        try await downloadFiles(entry)
        statuses[id] = .downloaded
    }

    public func load(id: String) async throws {
        _ = try await pipeline(for: id)
    }

    public func unload(id: String) {
        pipelines[id] = nil
        if statuses[id] == .ready { statuses[id] = .downloaded }
    }

    public func generate(
        id: String,
        prompt: String,
        negativePrompt: String,
        count: Int,
        steps: Int,
        guidanceScale: Float,
        seed: UInt32?
    ) async throws -> [GeneratedImage] {
        let pipeline = try await pipeline(for: id)
        let baseSeed = seed ?? UInt32.random(in: 0...UInt32.max)

        // Generation is CPU/GPU-bound and synchronous; keep the actor responsive.
        let images: [CGImage?] = try await Task.detached {
            var config = StableDiffusionPipeline.Configuration(prompt: prompt)
            config.negativePrompt = negativePrompt
            config.imageCount = count
            config.stepCount = steps
            config.guidanceScale = guidanceScale
            config.seed = baseSeed
            config.schedulerType = .dpmSolverMultistepScheduler
            config.disableSafety = true
            return try pipeline.generateImages(configuration: config) { _ in true }
        }.value

        return images.compactMap { cgImage -> GeneratedImage? in
            guard let cgImage else { return nil }
            let rep = NSBitmapImageRep(cgImage: cgImage)
            guard let data = rep.representation(using: .png, properties: [:]) else { return nil }
            return GeneratedImage(pngData: data, seed: baseSeed)
        }
    }

    // MARK: - Internals

    private func entry(for id: String) throws -> ModelRegistryEntry {
        guard let entry = registry.models.first(where: { $0.id == id }) else {
            throw ModelError.unknownModel(id)
        }
        guard entry.kind == .image else {
            throw ModelError.wrongKind(id, kind: entry.kind.rawValue)
        }
        return entry
    }

    private func pipeline(for id: String) async throws -> StableDiffusionPipeline {
        if let pipeline = pipelines[id] { return pipeline }
        let entry = try entry(for: id)
        do {
            try await downloadFiles(entry, implicit: true)
            statuses[id] = .loading
            let resources = resourcesURL(for: entry)
            let configuration = MLModelConfiguration()
            configuration.computeUnits = .cpuAndGPU // "original" attention variant
            let pipeline = try StableDiffusionPipeline(
                resourcesAt: resources,
                controlNet: [],
                configuration: configuration,
                disableSafety: true,
                reduceMemory: false
            )
            try pipeline.loadResources()
            pipelines[id] = pipeline
            statuses[id] = .ready
            return pipeline
        } catch {
            statuses[id] = .failed(error.localizedDescription)
            throw error
        }
    }

    /// The compiled variant folder holds the .mlmodelc set + vocab/merges directly.
    private func resourcesURL(for entry: ModelRegistryEntry) -> URL {
        let repoFolder = downloader.localRepoURL(entry.repo)
        // include globs select the variant (e.g. "original/compiled/*"); the first
        // path component pair is the variant directory.
        if let glob = entry.include.first {
            let components = glob.split(separator: "/").dropLast() // drop the "*" tail
            if !components.isEmpty {
                return components.reduce(repoFolder) { $0.appending(path: String($1)) }
            }
        }
        return repoFolder
    }

    private func downloadFiles(_ entry: ModelRegistryEntry, implicit: Bool = false) async throws {
        let resourcesPresent = FileManager.default.fileExists(
            atPath: resourcesURL(for: entry).appending(path: "Unet.mlmodelc").path)
            || FileManager.default.fileExists(
                atPath: resourcesURL(for: entry).appending(path: "UnetChunk1.mlmodelc").path)
        guard !resourcesPresent else {
            statuses[entry.id] = .downloaded
            return
        }
        if implicit && !settings.autoDownload {
            throw ModelError.autoDownloadDisabled(entry.id)
        }
        statuses[entry.id] = .downloading(progress: 0)
        try await downloader.downloadModelFiles(for: entry)
        statuses[entry.id] = .downloaded
    }
}
