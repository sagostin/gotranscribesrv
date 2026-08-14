import FluidAudio
import Vapor

/// Global engine manager — holds initialized FluidAudio models.
/// All inference is performed inside this actor to encapsulate
/// non-Sendable model instances.
///
/// NOTE: FluidAudio managers (AsrManager, SortformerDiarizer, etc.)
/// are non-Sendable classes. We store them as nonisolated(unsafe) because
/// their methods are thread-safe (internally synchronized via CoreML)
/// and we only ever call them from within this actor's context.
actor EngineManager {
    // MARK: - Engines
    // nonisolated(unsafe) allows calling nonisolated methods on these
    // from within the actor without triggering "sending" errors.

    private nonisolated(unsafe) var asrManager: AsrManager?
    private nonisolated(unsafe) var asrModels: AsrModels?
    private nonisolated(unsafe) var vadManager: VadManager?
    private nonisolated(unsafe) var sortformerDiarizer: SortformerDiarizer?
    private nonisolated(unsafe) var ttsManager: PocketTtsManager?
    private nonisolated(unsafe) var kokoroManager: KokoroAneManager?

    // MARK: - Configuration
    // Server-side TTS defaults — surfaced in /health and used as the
    // /synthesize fallback when no ?backend= is supplied.
    private var ttsDefaultBackend: String = "pocket"     // SIDECAR_TTS_DEFAULT_BACKEND
    private var ttsStreamBackend: String = "pocket"      // SIDECAR_TTS_STREAM_BACKEND (always pocket today)
    private var realtimeEngine: String = "eou-320"       // SIDECAR_REALTIME_ENGINE

    // MARK: - Status

    private var asrReady = false
    private var vadReady = false
    private var diarizerReady = false
    private var ttsReady = false
    private var kokoroReady = false

    /// Apply startup config from environment.
    /// Call once before `initialize()`.
    func applyConfig() {
        let defBack = Environment.get("SIDECAR_TTS_DEFAULT_BACKEND")?.lowercased() ?? "pocket"
        if defBack == "pocket" || defBack == "kokoro" {
            self.ttsDefaultBackend = defBack
        }
        let streamBack = Environment.get("SIDECAR_TTS_STREAM_BACKEND")?.lowercased() ?? "pocket"
        if streamBack == "pocket" {
            self.ttsStreamBackend = streamBack
        } else {
            // Kokoro streaming isn't supported in FluidAudio 0.15.5; ignore
            // a misconfigured env rather than silently misroute callers.
            print("⚠️  SIDECAR_TTS_STREAM_BACKEND=\(streamBack) ignored — only \"pocket\" supports streaming. Falling back to \"pocket\".")
            self.ttsStreamBackend = "pocket"
        }
        // Realtime engine — any of the 10 documented variants (eou-*, nemotron-*, unified-*)
        // is accepted. Unknown values pass through; the sidecar route handler will
        // surface a 400 with an "unknown engine" message if it doesn't match.
        let rt = Environment.get("SIDECAR_REALTIME_ENGINE")?.lowercased() ?? "eou-320"
        if !rt.isEmpty {
            self.realtimeEngine = rt
        }
    }

    func getTtsDefaultBackend() -> String { ttsDefaultBackend }
    func getTtsStreamBackend() -> String { ttsStreamBackend }
    func getRealtimeEngine() -> String { realtimeEngine }

    struct DefaultsSnapshot: Sendable {
        let synthesizeBackend: String
        let streamBackend: String
        let realtimeEngine: String
    }
    /// Snapshot of the server-side defaults — surfaced in /health.config.
    func getTtsDefaults() -> DefaultsSnapshot {
        DefaultsSnapshot(
            synthesizeBackend: ttsDefaultBackend,
            streamBackend: ttsStreamBackend,
            realtimeEngine: realtimeEngine
        )
    }

    /// Initialize all FluidAudio engines. Each degrades gracefully on failure.
    func initialize() async {
        // ASR — Parakeet TDT v3 (CoreML/ANE)
        do {
            let models = try await AsrModels.downloadAndLoad(version: .v3)
            let manager = AsrManager(config: .default)
            try await manager.loadModels(models)
            self.asrModels = models
            self.asrManager = manager
            self.asrReady = true
            print("✅ ASR engine loaded (Parakeet TDT v3, ANE)")
        } catch {
            print("⚠️  ASR engine failed to load: \(error)")
        }

        // VAD — Silero (CoreML/ANE)
        do {
            let manager = try await VadManager(config: VadConfig(defaultThreshold: 0.5))
            self.vadManager = manager
            self.vadReady = true
            print("✅ VAD engine loaded (Silero, ANE)")
        } catch {
            print("⚠️  VAD engine failed to load: \(error)")
        }

        // Diarization — Sortformer (NVIDIA, ANE)
        // Sortformer is ideal for phone calls: stable speaker IDs, handles noise well,
        // 4 speakers max (sufficient for phone calls), end-to-end neural (no clustering).
        do {
            let diarizer = SortformerDiarizer(config: .default)
            let models = try await SortformerModels.loadFromHuggingFace(config: .default)
            diarizer.initialize(models: models)
            self.sortformerDiarizer = diarizer
            self.diarizerReady = true
            print("✅ Diarizer loaded (Sortformer, ANE)")
        } catch {
            print("⚠️  Diarizer failed to load: \(error)")
        }

        // TTS — PocketTTS
        do {
            let manager = PocketTtsManager()
            try await manager.initialize()
            self.ttsManager = manager
            self.ttsReady = true
            print("✅ TTS engine loaded (PocketTTS, ANE)")
        } catch {
            print("⚠️  TTS engine failed to load: \(error)")
        }

        // TTS — Kokoro (ANE)
        // Optional alternative TTS backend. Higher quality + multilingual
        // (English/Mandarin/Japanese), but ~7 mlmodelcs + G2P + lexicon
        // — heavy first-time download. Loads non-fatally: sidecar stays
        // useful if Kokoro fails or is removed later.
        do {
            let manager = KokoroAneManager()
            try await manager.initialize()
            self.kokoroManager = manager
            self.kokoroReady = true
            print("✅ Kokoro TTS loaded (ANE)")
        } catch {
            print("⚠️  Kokoro TTS failed to load: \(error)")
        }
    }

    // MARK: - Inference Methods

    func transcribe(_ samples: [Float]) async throws -> ASRResult {
        guard let manager = asrManager else {
            throw Abort(.serviceUnavailable, reason: "ASR engine not loaded")
        }
        // FluidAudio 0.15.x requires an explicit TDT decoder state.
        // Each request is independent, so a fresh state per call preserves
        // the pre-0.15 stateless semantics exactly.
        var decoderState = TdtDecoderState.make()
        return try await manager.transcribe(samples, decoderState: &decoderState)
    }

    func vadSegment(_ samples: [Float], config: VadSegmentationConfig) async throws -> [(startTime: Double, endTime: Double)] {
        guard let manager = vadManager else {
            throw Abort(.serviceUnavailable, reason: "VAD engine not loaded")
        }
        let segments = try await manager.segmentSpeech(samples, config: config)
        return segments.map { (startTime: $0.startTime, endTime: $0.endTime) }
    }

    /// Process one 256ms (4096-sample) streaming VAD chunk.
    /// Used by /stream/realtime to emit speech_started / speech_stopped events.
    func vadStreamingChunk(_ chunk: [Float], state: VadStreamState) async throws -> VadStreamResult {
        guard let manager = vadManager else {
            throw Abort(.serviceUnavailable, reason: "VAD engine not loaded")
        }
        return try await manager.processStreamingChunk(
            chunk, state: state, returnSeconds: true)
    }

    /// Run Sortformer diarization on a complete audio buffer.
    /// Returns a flat array of (speakerIndex, startTime, endTime) tuples.
    func diarize(_ samples: [Float]) throws -> [(speakerIndex: Int, startTime: Float, endTime: Float)] {
        guard let diarizer = sortformerDiarizer else {
            throw Abort(.serviceUnavailable, reason: "Diarization engine not loaded")
        }

        // Reset state for fresh processing
        diarizer.reset()

        // Process complete audio file — returns DiarizerTimeline
        let timeline = try diarizer.processComplete(samples)

        // Flatten segments from all speakers into a single sorted array.
        // timeline.speakers is [speakerIndex: DiarizerSpeaker]
        var flatSegments: [(speakerIndex: Int, startTime: Float, endTime: Float)] = []
        for (speakerIdx, speaker) in timeline.speakers {
            for seg in speaker.finalizedSegments {
                flatSegments.append((
                    speakerIndex: speakerIdx,
                    startTime: seg.startTime,
                    endTime: seg.endTime
                ))
            }
        }

        // Sort by start time for efficient dominant-speaker lookup
        flatSegments.sort { $0.startTime < $1.startTime }

        return flatSegments
    }

    func hasDiarizer() -> Bool {
        diarizerReady
    }

    func synthesize(text: String, voice: String? = nil) async throws -> Data {
        guard let manager = ttsManager else {
            throw Abort(.serviceUnavailable, reason: "TTS engine not loaded")
        }
        return try await manager.synthesize(text: text, voice: voice)
    }

    func synthesizeWithClone(text: String, voiceURL: URL) async throws -> Data {
        guard let manager = ttsManager else {
            throw Abort(.serviceUnavailable, reason: "TTS engine not loaded")
        }
        let voiceData = try await manager.cloneVoice(from: voiceURL)
        return try await manager.synthesize(text: text, voiceData: voiceData)
    }

    /// Extract a voice embedding from an audio file without synthesis.
    /// Returns serialized voice data (raw Float32 binary) that can be stored and reused.
    func extractVoiceEmbedding(audioURL: URL) async throws -> Data {
        guard let manager = ttsManager else {
            throw Abort(.serviceUnavailable, reason: "TTS engine not loaded")
        }
        let voiceData = try await manager.cloneVoice(from: audioURL)

        // Serialize PocketTtsVoiceData → raw Float32 binary via temp file
        let tempURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("voice-embed-\(UUID().uuidString).bin")
        defer { try? FileManager.default.removeItem(at: tempURL) }
        try manager.saveClonedVoice(voiceData, to: tempURL)
        return try Data(contentsOf: tempURL)
    }

    /// Synthesize speech using a pre-extracted voice embedding (raw Float32 binary).
    func synthesizeWithEmbedding(text: String, voiceData: Data) async throws -> Data {
        guard let manager = ttsManager else {
            throw Abort(.serviceUnavailable, reason: "TTS engine not loaded")
        }

        // Deserialize raw Float32 binary → PocketTtsVoiceData via temp file
        let tempURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("voice-load-\(UUID().uuidString).bin")
        defer { try? FileManager.default.removeItem(at: tempURL) }
        try voiceData.write(to: tempURL)
        let pocketVoice = try manager.loadClonedVoice(from: tempURL)
        return try await manager.synthesize(text: text, voiceData: pocketVoice)
    }

    // MARK: - Kokoro TTS (alternative backend)

    func synthesizeKokoro(text: String, voice: String? = nil) async throws -> Data {
        guard let manager = kokoroManager else {
            throw Abort(.serviceUnavailable, reason: "Kokoro TTS engine not loaded")
        }
        let speed = Float(1.0)
        return try await manager.synthesize(text: text, voice: voice, speed: speed)
    }

    func hasKokoro() -> Bool { kokoroReady }

    /// Returns an AsyncThrowingStream of int16 PCM frames (24kHz mono, L16).
    /// Used by /synthesize/stream for low-latency streaming TTS output.
    /// AudioFrame type is from PocketTtsManager; cast to Data via the
    /// frame's Int16 buffer.
    func pocketSynthesizeStream(
        text: String, voice: String?
    ) async throws -> AsyncThrowingStream<PocketTtsSynthesizer.AudioFrame, Error> {
        guard let manager = ttsManager else {
            throw Abort(.serviceUnavailable, reason: "TTS engine not loaded")
        }
        return try await manager.synthesizeStreaming(text: text, voice: voice)
    }

    func healthStatus() -> [String: String] {
        [
            "asr": asrReady ? "loaded" : "not loaded",
            "vad": vadReady ? "loaded" : "not loaded",
            "diarizer": diarizerReady ? "loaded" : "not loaded",
            "tts": ttsReady ? "loaded" : "not loaded",
            "kokoro": kokoroReady ? "loaded" : "not loaded",
        ]
    }
}
