import FluidAudio
import Vapor

/// Outcome of resolving a client-supplied voice against the known catalogs.
struct ResolvedVoice: Sendable {
    /// Backend that will actually run synthesis ("pocket" or "kokoro").
    let backend: String
    /// Voice to pass to the backend — nil means "use the backend's built-in
    /// default" (PocketTTS: alba, Kokoro EN: af_heart).
    let voice: String?
    /// Resolved voice name for headers/logging (never nil — defaults resolved).
    let effectiveVoice: String
    /// Human-readable note when an alias/reroute was applied (nil = exact match).
    let note: String?
}

/// Backend-aware voice resolution for the TTS routes.
///
/// Why this exists: clients (including the Go gateway) send `voice: "default"`
/// or omit the voice entirely. FluidAudio only applies its built-in default
/// when the voice is `nil` — a literal "default" is treated as a voice name
/// and triggers a HuggingFace download of a nonexistent `default.safetensors`
/// (404), which previously surfaced as an opaque 500 → gateway 502.
///
/// Resolution rules:
///  1. nil / empty / "default" (any case) → backend's built-in default.
///  2. Voice known to the *other* backend → reroute when the backend came
///     from the server default; 422 when `?backend=` was explicit.
///  3. Unknown voice → 422 listing the valid voices per backend.
enum VoiceResolver {

    /// Canonical PocketTTS voice catalog (advertised in /voices).
    static let pocketVoiceCatalog: [(id: String, name: String, description: String)] = [
        ("jane", "Jane", "Female, conversational"),
        ("alba", "Alba", "Male, reading & conversational"),
        ("charles", "Charles", "Male, conversational"),
        ("anna", "Anna", "Female, conversational"),
        ("eve", "Eve", "Female, conversational"),
        ("george", "George", "Male, conversational"),
        ("paul", "Paul", "Male, conversational"),
        ("mary", "Mary", "Female, conversational"),
        ("michael", "Michael", "Male, conversational"),
        ("vera", "Vera", "Female, conversational"),
        ("jean", "Jean", "Male, conversational"),
        ("eponine", "Eponine", "Female, reading"),
        ("fantine", "Fantine", "Female, reading"),
        ("marius", "Marius", "Male"),
        ("cosette", "Cosette", "Female"),
        ("azelma", "Azelma", "Female, reading"),
    ]

    static let pocketVoices: Set<String> = Set(pocketVoiceCatalog.map { $0.id })

    /// Curated Kokoro voice catalog — single source of truth (also used by
    /// /voices). Kokoro voice packs auto-download on first use; we only list
    /// IDs that resolve reliably for the configured variant.
    static let kokoroVoiceCatalog: [(id: String, name: String, description: String)] = [
        ("af_heart", "Heart (EN)", "Female, warm — Kokoro default"),
        ("af_bella", "Bella (EN)", "Female"),
        ("af_sky", "Sky (EN)", "Female"),
        ("af_nicole", "Nicole (EN)", "Female, news"),
        ("am_adam", "Adam (EN)", "Male"),
        ("am_michael", "Michael (EN)", "Male"),
        ("bf_emma", "Emma (EN)", "Female, British"),
        ("bf_isabella", "Isabella (EN)", "Female, British"),
        ("bm_george", "George (EN)", "Male, British"),
        ("zf_001", "Xiaoxiao (ZH)", "Female, Mandarin default"),
        ("zf_002", "Xiaoyi (ZH)", "Female, Mandarin"),
        ("zm_001", "Yunjian (ZH)", "Male, Mandarin"),
        ("jf_alpha", "Alpha (JA)", "Female, Japanese default"),
    ]

    static let kokoroVoiceIDs: Set<String> = Set(kokoroVoiceCatalog.map { $0.id })

    /// Default voice names per backend (matching the managers' configuration:
    /// PocketTtsManager default + KokoroAneManager english variant default).
    static func defaultVoice(for backend: String) -> String {
        backend == "kokoro" ? KokoroAneConstants.defaultVoice : PocketTtsConstants.defaultVoice
    }

    /// Pocket voices discovered on disk in the FluidAudio cache — covers
    /// upstream voices beyond the advertised set that are already downloaded
    /// (e.g. LibriVox extras like stuart_bell). Scanned per call; a single
    /// local directory listing is negligible next to TTS synthesis.
    static func pocketVoicesOnDisk() -> Set<String> {
        var found = Set<String>()
        guard let cacheRoot = try? TtsCacheDirectory.ensure() else { return found }
        let constantsDir =
            cacheRoot
            .appendingPathComponent(PocketTtsConstants.defaultModelsSubdirectory)
            .appendingPathComponent(Repo.pocketTts.folderName)
            .appendingPathComponent(PocketTtsLanguage.english.repoSubdirectory)
            .appendingPathComponent(ModelNames.PocketTTS.constantsBinDir)
        guard let contents = try? FileManager.default.contentsOfDirectory(
            at: constantsDir, includingPropertiesForKeys: nil
        ) else { return found }
        for file in contents where file.pathExtension == "safetensors" {
            found.insert(file.deletingPathExtension().lastPathComponent)
        }
        return found
    }

    static func isPocketVoice(_ voice: String) -> Bool {
        pocketVoices.contains(voice) || pocketVoicesOnDisk().contains(voice)
    }

    /// Resolve a client-supplied voice for the given backend.
    ///
    /// - Parameters:
    ///   - voice: Raw voice from the request body (may be nil/empty/"default").
    ///   - backend: "pocket" or "kokoro" (already validated).
    ///   - backendExplicit: true when the caller passed `?backend=` themselves.
    ///     Explicit intent wins over auto-rerouting.
    ///   - kokoroLoaded: whether the Kokoro engine initialized at startup.
    ///   - logger: request logger for alias/reroute/rejection audit lines.
    static func resolve(
        voice: String?,
        backend: String,
        backendExplicit: Bool,
        kokoroLoaded: Bool,
        logger: Logger
    ) throws -> ResolvedVoice {
        let trimmed = (voice ?? "").trimmingCharacters(in: .whitespacesAndNewlines)

        // Rule 1 — nil/empty/"default" → backend's built-in default.
        if trimmed.isEmpty || trimmed.lowercased() == "default" {
            let note = trimmed.lowercased() == "default"
                ? "voice alias \"default\" → \(backend) default (\(defaultVoice(for: backend)))"
                : nil
            if let note { logger.info("\(note)") }
            return ResolvedVoice(
                backend: backend, voice: nil,
                effectiveVoice: defaultVoice(for: backend), note: note)
        }

        let name = trimmed
        let pocket = isPocketVoice(name)
        let kokoro = kokoroVoiceIDs.contains(name)

        // Rule 2 — known voice, wrong backend.
        if pocket && backend == "kokoro" {
            if backendExplicit {
                throw Abort(
                    .unprocessableEntity,
                    reason: "voice \"\(name)\" is a PocketTTS voice — re-send with ?backend=pocket or omit backend."
                )
            }
            logger.info("rerouting voice \"\(name)\" kokoro → pocket (voice belongs to pocket)")
            return ResolvedVoice(
                backend: "pocket", voice: name, effectiveVoice: name,
                note: "rerouted kokoro → pocket")
        }
        if kokoro && backend == "pocket" {
            if backendExplicit {
                throw Abort(
                    .unprocessableEntity,
                    reason: "voice \"\(name)\" is a Kokoro voice — re-send with ?backend=kokoro or omit backend."
                )
            }
            guard kokoroLoaded else {
                throw Abort(
                    .serviceUnavailable,
                    reason: "voice \"\(name)\" requires the Kokoro backend, which failed to load on this server."
                )
            }
            logger.info("rerouting voice \"\(name)\" pocket → kokoro (voice belongs to kokoro)")
            return ResolvedVoice(
                backend: "kokoro", voice: name, effectiveVoice: name,
                note: "rerouted pocket → kokoro")
        }

        // Rule 3 — unknown voice.
        guard pocket || kokoro else {
            logger.warning("unknown voice \"\(name)\" requested (backend=\(backend))")
            throw Abort(
                .unprocessableEntity,
                reason: """
                    unknown voice "\(name)". Valid pocket voices: \(pocketVoices.sorted().joined(separator: ", ")). \
                    Valid kokoro voices: \(kokoroVoiceIDs.sorted().joined(separator: ", ")). \
                    Use "default" or omit voice for the backend default.
                    """
            )
        }

        // Known voice, correct backend. Kokoro selected but engine down?
        if backend == "kokoro" && !kokoroLoaded {
            throw Abort(
                .serviceUnavailable,
                reason: "Kokoro backend is not loaded on this server — use ?backend=pocket or a pocket voice."
            )
        }
        return ResolvedVoice(backend: backend, voice: name, effectiveVoice: name, note: nil)
    }
}
