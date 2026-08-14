import Foundation

/// What a registry entry is for.
public enum ModelKind: String, Codable, Sendable {
    case chat
    case image
    case embedding
}

/// Inference backend for chat entries.
public enum ModelRuntime: String, Codable, Sendable {
    /// Our own stateful CoreML runner (swift-transformers LanguageModel family).
    case standard
    /// External runtime (CoreML-LLM package) — needed for bespoke repos like
    /// mlboydaisuke/gemma-4-E2B-coreml whose format is not the exporters convention.
    case coremlLLM = "coreml-llm"
}

/// One entry in `models.json`: where to get a model and how to serve it.
    public struct ModelRegistryEntry: Codable, Sendable {
        /// Local identifier used in API requests (`model` field).
        public var id: String
        public var kind: ModelKind = .chat
        /// Hugging Face repo id, e.g. "apple/mistral-coreml".
        public var repo: String
        /// Backend for chat entries.
        public var runtime: ModelRuntime = .standard
        /// Glob patterns selecting which files to download from the repo.
        public var include: [String]
        /// Explicit .mlpackage/.mlmodelc directory name inside the repo.
        /// If nil, the first model package found (sorted) is used.
        public var packageName: String?
        /// Repo to fetch config.json / tokenizer files from. Defaults to `repo`.
        /// Point this at the base model repo for community CoreML conversions.
        public var tokenizerRepo: String?
        /// Download + load + warm up at server start.
        public var preload: Bool = false
        /// Default generation cap per request (chat models).
        public var maxNewTokens: Int = 512
        /// Embedding architecture for kind=embedding ("bert", "modernbert", "nomicbert").
        public var architecture: String?
        /// Native image size for kind=image (512 for SD 1.x/2.x, 1024 for SDXL).
        public var imageSize: Int?
        /// Free-form notes (gated repo, quirks, etc).
        public var notes: String?

        public init(
            id: String, kind: ModelKind = .chat, repo: String, runtime: ModelRuntime = .standard,
            include: [String], packageName: String? = nil, tokenizerRepo: String? = nil,
            preload: Bool = false, maxNewTokens: Int = 512, architecture: String? = nil,
            imageSize: Int? = nil, notes: String? = nil
        ) {
            self.id = id
            self.kind = kind
            self.repo = repo
            self.runtime = runtime
            self.include = include
            self.packageName = packageName
            self.tokenizerRepo = tokenizerRepo
            self.preload = preload
            self.maxNewTokens = maxNewTokens
            self.architecture = architecture
            self.imageSize = imageSize
            self.notes = notes
        }

        // Custom decoding so defaulted fields are genuinely optional in models.json
        // (synthesized Codable requires every non-optional key).
        public init(from decoder: Decoder) throws {
            let container = try decoder.container(keyedBy: CodingKeys.self)
            id = try container.decode(String.self, forKey: .id)
            kind = try container.decodeIfPresent(ModelKind.self, forKey: .kind) ?? .chat
            repo = try container.decode(String.self, forKey: .repo)
            runtime = try container.decodeIfPresent(ModelRuntime.self, forKey: .runtime) ?? .standard
            include = try container.decodeIfPresent([String].self, forKey: .include) ?? []
            packageName = try container.decodeIfPresent(String.self, forKey: .packageName)
            tokenizerRepo = try container.decodeIfPresent(String.self, forKey: .tokenizerRepo)
            preload = try container.decodeIfPresent(Bool.self, forKey: .preload) ?? false
            maxNewTokens = try container.decodeIfPresent(Int.self, forKey: .maxNewTokens) ?? 512
            architecture = try container.decodeIfPresent(String.self, forKey: .architecture)
            imageSize = try container.decodeIfPresent(Int.self, forKey: .imageSize)
            notes = try container.decodeIfPresent(String.self, forKey: .notes)
        }
    }

/// Server-wide settings, from `models.json` "settings" (env vars override).
public struct ServerSettings: Codable, Sendable {
    public struct Features: Codable, Sendable {
        /// Master switch for image generation (models never download/load, route 403s).
        public var images: Bool = true
        /// Master switch for embeddings.
        public var embeddings: Bool = true

        public init(images: Bool = true, embeddings: Bool = true) {
            self.images = images
            self.embeddings = embeddings
        }

        public init(from decoder: Decoder) throws {
            let container = try decoder.container(keyedBy: CodingKeys.self)
            images = try container.decodeIfPresent(Bool.self, forKey: .images) ?? true
            embeddings = try container.decodeIfPresent(Bool.self, forKey: .embeddings) ?? true
        }
    }

    /// If false, models are never downloaded implicitly: requests for un-downloaded
    /// models fail with 409. Explicit POST /models/:id/download still works.
    public var autoDownload: Bool = true
    /// Master switch for boot-time preloading (per-entry `preload` ANDs with this).
    public var preload: Bool = true
    public var features: Features = .init()

    public init(autoDownload: Bool = true, preload: Bool = true, features: Features = .init()) {
        self.autoDownload = autoDownload
        self.preload = preload
        self.features = features
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        autoDownload = try container.decodeIfPresent(Bool.self, forKey: .autoDownload) ?? true
        preload = try container.decodeIfPresent(Bool.self, forKey: .preload) ?? true
        features = try container.decodeIfPresent(Features.self, forKey: .features) ?? .init()
    }

    /// Apply environment overrides: COREML_AUTO_DOWNLOAD, COREML_PRELOAD,
    /// COREML_IMAGES, COREML_EMBEDDINGS ("0"/"false"/"no" disable).
    public func applyingEnvironment(_ env: [String: String] = ProcessInfo.processInfo.environment) -> ServerSettings {
        func flag(_ key: String) -> Bool? {
            guard let value = env[key]?.lowercased() else { return nil }
            return !["0", "false", "no", "off"].contains(value)
        }
        var copy = self
        if let v = flag("COREML_AUTO_DOWNLOAD") { copy.autoDownload = v }
        if let v = flag("COREML_PRELOAD") { copy.preload = v }
        if let v = flag("COREML_IMAGES") { copy.features.images = v }
        if let v = flag("COREML_EMBEDDINGS") { copy.features.embeddings = v }
        return copy
    }
}

public struct ModelRegistry: Codable, Sendable {
    public var settings: ServerSettings = .init()
    public var models: [ModelRegistryEntry]

    public init(models: [ModelRegistryEntry], settings: ServerSettings = .init()) {
        self.models = models
        self.settings = settings
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        models = try container.decodeIfPresent([ModelRegistryEntry].self, forKey: .models) ?? []
        settings = try container.decodeIfPresent(ServerSettings.self, forKey: .settings) ?? .init()
    }

    public static func load(from url: URL) throws -> ModelRegistry {
        let data = try Data(contentsOf: url)
        return try JSONDecoder().decode(ModelRegistry.self, from: data)
    }

    public func entries(kind: ModelKind) -> [ModelRegistryEntry] {
        models.filter { $0.kind == kind }
    }
}
