import FluidAudio
import Vapor

/// TTS routes:
/// - POST /synthesize — Text-to-speech synthesis
/// - POST /clone-voice — Extract voice embedding from audio
/// - GET /voices — List available voice presets (system + filesystem)
func ttsRoutes(_ app: Application, engines: EngineManager) {
    app.post("synthesize") { req async throws -> Response in
        try await handleSynthesize(req: req, engines: engines)
    }

    app.post("clone-voice") { req async throws -> Response in
        try await handleCloneVoice(req: req, engines: engines)
    }

    app.get("voices") { req async throws -> VoicesListResponse in
        handleListVoices()
    }
}

private func handleSynthesize(req: Request, engines: EngineManager) async throws -> Response {
    let body = try req.content.decode(SynthesizeRequest.self)

    let audioData: Data

    if let voiceData = body.voice_data, !voiceData.isEmpty {
        // Pre-extracted voice embedding — fastest path (stored voices)
        guard let embeddingBytes = Data(base64Encoded: voiceData) else {
            throw Abort(.badRequest, reason: "Invalid base64 voice_data")
        }
        audioData = try await engines.synthesizeWithEmbedding(text: body.text, voiceData: embeddingBytes)
    } else if let voiceRef = body.voice_ref, !voiceRef.isEmpty {
        // Raw audio reference — one-shot voice cloning
        guard let refBytes = Data(base64Encoded: voiceRef) else {
            throw Abort(.badRequest, reason: "Invalid base64 voice reference")
        }

        let refURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("voice-ref-\(UUID().uuidString).wav")
        defer { try? FileManager.default.removeItem(at: refURL) }

        let refSamples = try await SidecarAudioConverter.toPCM16kMono(refBytes)
        try writeWav(samples: refSamples, sampleRate: 16000, to: refURL)

        // Voice cloning + synthesis (runs inside actor)
        audioData = try await engines.synthesizeWithClone(text: body.text, voiceURL: refURL)
    } else {
        // Named system voice (runs inside actor)
        audioData = try await engines.synthesize(text: body.text, voice: body.voice)
    }

    var headers = HTTPHeaders()
    headers.add(name: .contentType, value: "audio/wav")
    headers.add(name: "X-Audio-Sample-Rate", value: "24000")
    return Response(status: .ok, headers: headers, body: .init(data: audioData))
}

/// Extract a voice embedding from uploaded audio.
/// Returns raw embedding bytes that can be stored and reused in voice_data.
private func handleCloneVoice(req: Request, engines: EngineManager) async throws -> Response {
    // Accept multipart audio upload
    guard let audioFile = try? req.content.decode(CloneVoiceRequest.self) else {
        // Try raw body as audio
        guard let body = req.body.data else {
            throw Abort(.badRequest, reason: "Audio file required")
        }
        let audioBytes = Data(buffer: body)
        return try await extractEmbedding(audioBytes: audioBytes, engines: engines)
    }

    return try await extractEmbedding(audioBytes: Data(buffer: audioFile.audio.data), engines: engines)
}

private func extractEmbedding(audioBytes: Data, engines: EngineManager) async throws -> Response {
    let tempURL = FileManager.default.temporaryDirectory
        .appendingPathComponent("clone-\(UUID().uuidString).wav")
    defer { try? FileManager.default.removeItem(at: tempURL) }

    let samples = try await SidecarAudioConverter.toPCM16kMono(audioBytes)
    try writeWav(samples: samples, sampleRate: 16000, to: tempURL)

    // Calculate actual audio duration from sample count (16kHz mono)
    let audioDurationMs = Int(Double(samples.count) / 16000.0 * 1000.0)

    let embedding: Data
    do {
        embedding = try await engines.extractVoiceEmbedding(audioURL: tempURL)
    } catch let error as PocketTTSError {
        // Surface the actual PocketTTS error (e.g. "Audio too long", "Audio too short")
        throw Abort(.unprocessableEntity, reason: "\(error)")
    }

    var headers = HTTPHeaders()
    headers.add(name: .contentType, value: "application/octet-stream")
    headers.add(name: "Content-Length", value: "\(embedding.count)")
    headers.add(name: "X-Audio-Duration-Ms", value: "\(audioDurationMs)")
    return Response(status: .ok, headers: headers, body: .init(data: embedding))
}

private func handleListVoices() -> VoicesListResponse {
    // PocketTTS built-in voices
    let systemVoices: [VoiceInfo] = [
        VoiceInfo(id: "default", name: "default", description: "PocketTTS default voice", type: "system"),
        VoiceInfo(id: "jane", name: "Jane", description: "Female, conversational", type: "system"),
        VoiceInfo(id: "alba", name: "Alba", description: "Male, reading & conversational", type: "system"),
        VoiceInfo(id: "charles", name: "Charles", description: "Male, conversational", type: "system"),
        VoiceInfo(id: "anna", name: "Anna", description: "Female, conversational", type: "system"),
        VoiceInfo(id: "eve", name: "Eve", description: "Female, conversational", type: "system"),
        VoiceInfo(id: "george", name: "George", description: "Male, conversational", type: "system"),
        VoiceInfo(id: "paul", name: "Paul", description: "Male, conversational", type: "system"),
        VoiceInfo(id: "mary", name: "Mary", description: "Female, conversational", type: "system"),
        VoiceInfo(id: "michael", name: "Michael", description: "Male, conversational", type: "system"),
        VoiceInfo(id: "vera", name: "Vera", description: "Female, conversational", type: "system"),
        VoiceInfo(id: "jean", name: "Jean", description: "Male, conversational", type: "system"),
        VoiceInfo(id: "eponine", name: "Eponine", description: "Female, reading", type: "system"),
        VoiceInfo(id: "fantine", name: "Fantine", description: "Female, reading", type: "system"),
        VoiceInfo(id: "marius", name: "Marius", description: "Male", type: "system"),
        VoiceInfo(id: "cosette", name: "Cosette", description: "Female", type: "system"),
        VoiceInfo(id: "azelma", name: "Azelma", description: "Female, reading", type: "system"),
    ]

    // Also scan filesystem for any .wav presets
    var voices = systemVoices
    let voicesDir = URL(fileURLWithPath: "voices")
    if FileManager.default.fileExists(atPath: voicesDir.path) {
        if let contents = try? FileManager.default.contentsOfDirectory(
            at: voicesDir, includingPropertiesForKeys: nil
        ) {
            for file in contents where file.pathExtension == "wav" {
                let id = file.deletingPathExtension().lastPathComponent
                if !voices.contains(where: { $0.id == id }) {
                    voices.append(VoiceInfo(
                        id: id,
                        name: file.deletingPathExtension().lastPathComponent,
                        description: "File-based voice preset",
                        type: "system"
                    ))
                }
            }
        }
    }

    return VoicesListResponse(voices: voices)
}

private func writeWav(samples: [Float], sampleRate: Int, to url: URL) throws {
    var data = Data()

    let numSamples = samples.count
    let dataSize = numSamples * 2
    let fileSize = 36 + dataSize

    data.append(contentsOf: "RIFF".utf8)
    withUnsafeBytes(of: UInt32(fileSize).littleEndian) { data.append(contentsOf: $0) }
    data.append(contentsOf: "WAVE".utf8)
    data.append(contentsOf: "fmt ".utf8)
    withUnsafeBytes(of: UInt32(16).littleEndian) { data.append(contentsOf: $0) }
    withUnsafeBytes(of: UInt16(1).littleEndian) { data.append(contentsOf: $0) }
    withUnsafeBytes(of: UInt16(1).littleEndian) { data.append(contentsOf: $0) }
    withUnsafeBytes(of: UInt32(sampleRate).littleEndian) { data.append(contentsOf: $0) }
    withUnsafeBytes(of: UInt32(sampleRate * 2).littleEndian) { data.append(contentsOf: $0) }
    withUnsafeBytes(of: UInt16(2).littleEndian) { data.append(contentsOf: $0) }
    withUnsafeBytes(of: UInt16(16).littleEndian) { data.append(contentsOf: $0) }
    data.append(contentsOf: "data".utf8)
    withUnsafeBytes(of: UInt32(dataSize).littleEndian) { data.append(contentsOf: $0) }

    for sample in samples {
        let clamped = max(-1.0, min(1.0, sample))
        let int16 = Int16(clamped * 32767)
        withUnsafeBytes(of: int16.littleEndian) { data.append(contentsOf: $0) }
    }

    try data.write(to: url)
}

// MARK: - Models

struct SynthesizeRequest: Content {
    let text: String
    var voice: String?
    var voice_ref: String?    // Base64 raw audio for one-shot cloning
    var voice_data: String?   // Base64 pre-extracted embedding for stored voices
    var speed: Double?
    var format: String?
}

struct CloneVoiceRequest: Content {
    let audio: File
}

struct VoicesListResponse: Content {
    let voices: [VoiceInfo]
}

struct VoiceInfo: Content {
    let id: String
    let name: String?
    let description: String?
    let type: String?
}
