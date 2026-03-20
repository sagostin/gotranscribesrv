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

    // MARK: - Status

    private var asrReady = false
    private var vadReady = false
    private var diarizerReady = false
    private var ttsReady = false

    /// Initialize all FluidAudio engines. Each degrades gracefully on failure.
    func initialize() async {
        // ASR — Parakeet TDT v3 (CoreML/ANE)
        do {
            let models = try await AsrModels.downloadAndLoad(version: .v3)
            let manager = AsrManager(config: .default)
            try await manager.initialize(models: models)
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
    }

    // MARK: - Inference Methods

    func transcribe(_ samples: [Float]) async throws -> ASRResult {
        guard let manager = asrManager else {
            throw Abort(.serviceUnavailable, reason: "ASR engine not loaded")
        }
        return try await manager.transcribe(samples)
    }

    func vadSegment(_ samples: [Float], config: VadSegmentationConfig) async throws -> [(startTime: Double, endTime: Double)] {
        guard let manager = vadManager else {
            throw Abort(.serviceUnavailable, reason: "VAD engine not loaded")
        }
        let segments = try await manager.segmentSpeech(samples, config: config)
        return segments.map { (startTime: $0.startTime, endTime: $0.endTime) }
    }

    /// Run Sortformer diarization on a complete audio buffer.
    /// Returns a flat array of (speakerIndex, startTime, endTime) tuples.
    func diarize(_ samples: [Float]) throws -> [(speakerIndex: Int, startTime: Float, endTime: Float)] {
        guard let diarizer = sortformerDiarizer else {
            throw Abort(.serviceUnavailable, reason: "Diarization engine not loaded")
        }

        // Reset state for fresh processing
        diarizer.reset()

        // Process complete audio file — returns SortformerTimeline
        let timeline = try diarizer.processComplete(samples)

        // Flatten segments from all speakers into a single sorted array.
        // timeline.segments is [[SortformerSegment]] — indexed by speaker slot (0-3).
        var flatSegments: [(speakerIndex: Int, startTime: Float, endTime: Float)] = []
        for (speakerIdx, speakerSegments) in timeline.segments.enumerated() {
            for seg in speakerSegments {
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

    func healthStatus() -> [String: String] {
        [
            "asr": asrReady ? "loaded" : "not loaded",
            "vad": vadReady ? "loaded" : "not loaded",
            "diarizer": diarizerReady ? "loaded" : "not loaded",
            "tts": ttsReady ? "loaded" : "not loaded",
        ]
    }
}
