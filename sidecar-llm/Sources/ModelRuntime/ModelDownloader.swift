import Foundation
@preconcurrency import Hub

/// Wraps swift-transformers' HubApi to snapshot model repos into a local directory.
/// Immutable after init (HubApi is all-let value types), hence unchecked Sendable.
public struct ModelDownloader: @unchecked Sendable {
    public let hub: HubApi
    public let downloadBase: URL

    public init(downloadBase: URL, hfToken: String? = nil) {
        self.hub = HubApi(downloadBase: downloadBase, hfToken: hfToken)
        self.downloadBase = downloadBase
    }

    /// Local folder for a downloaded repo snapshot.
    public func localRepoURL(_ repoId: String) -> URL {
        hub.localRepoLocation(Hub.Repo(id: repoId))
    }

    /// Download (or reuse) the model package files for a registry entry.
    /// Returns the local repo folder.
    @discardableResult
    public func downloadModelFiles(
        for entry: ModelRegistryEntry,
        progress: @escaping @Sendable (Double) -> Void = { _ in }
    ) async throws -> URL {
        try await hub.snapshot(from: Hub.Repo(id: entry.repo), matching: entry.include) { p in
            progress(p.fractionCompleted)
        }
        return localRepoURL(entry.repo)
    }

    /// Download (or reuse) config/tokenizer files. Returns the folder containing them.
    @discardableResult
    public func downloadTokenizerFiles(for entry: ModelRegistryEntry) async throws -> URL {
        let repo = entry.tokenizerRepo ?? entry.repo
        let files = ["config.json", "tokenizer_config.json", "tokenizer.json", "special_tokens_map.json"]
        return try await hub.snapshot(from: Hub.Repo(id: repo), matching: files)
    }

    /// Locate the CoreML package inside a downloaded repo folder.
    public func findPackage(in repoFolder: URL, entry: ModelRegistryEntry) throws -> URL {
        let fm = FileManager.default
        if let packageName = entry.packageName {
            let url = repoFolder.appending(path: packageName)
            guard fm.fileExists(atPath: url.path) else {
                throw ModelError.packageNotFound(entry.id)
            }
            return url
        }
        let contents = try fm.contentsOfDirectory(atPath: repoFolder.path).sorted()
        guard let name = contents.first(where: {
            ["mlpackage", "mlmodelc", "mlmodel"].contains($0.split(separator: ".").last.map(String.init) ?? "")
        }) else {
            throw ModelError.packageNotFound(entry.id)
        }
        return repoFolder.appending(path: name)
    }
}

public enum ModelError: Error, CustomStringConvertible {
    case unknownModel(String)
    case packageNotFound(String)
    case downloadFailed(String)
    case promptTooLong(model: String)
    case incompatibleLayout(String, inputs: String)
    case wrongKind(String, kind: String)
    case autoDownloadDisabled(String)
    case unsupportedRuntime(String, runtime: String)

    public var description: String {
        switch self {
        case .unknownModel(let id): return "Unknown model id: \(id)"
        case .packageNotFound(let id): return "No .mlpackage/.mlmodelc/.mlmodel found for model: \(id)"
        case .downloadFailed(let msg): return "Download failed: \(msg)"
        case .promptTooLong(let id): return "Prompt exceeds the model's context length: \(id)"
        case .incompatibleLayout(let id, let inputs):
            return "Model '\(id)' does not follow a supported convention (expected 'inputIds' input with 'logits' or top-k outputs; found: \(inputs)). Bespoke community conversions need conversion or a dedicated adapter."
        case .wrongKind(let id, let kind): return "Model '\(id)' is kind '\(kind)', not a chat model"
        case .autoDownloadDisabled(let id):
            return "Model '\(id)' is not downloaded and autoDownload is disabled. Run POST /models/\(id)/download first."
        case .unsupportedRuntime(let id, let runtime):
            return "Model '\(id)' uses runtime '\(runtime)' which is not served by this endpoint."
        }
    }
}
