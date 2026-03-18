import FluidAudio
import Vapor

/// TTS routes:
/// - POST /synthesize — Text-to-speech synthesis
/// - GET /voices — List available voice presets
func ttsRoutes(_ app: Application, engines: EngineManager) {
    app.post("synthesize") { req async throws -> Response in
        try await handleSynthesize(req: req, engines: engines)
    }

    app.get("voices") { req async throws -> VoicesListResponse in
        handleListVoices()
    }
}

private func handleSynthesize(req: Request, engines: EngineManager) async throws -> Response {
    let body = try req.content.decode(SynthesizeRequest.self)

    let audioData: Data

    if let voiceRef = body.voice_ref, !voiceRef.isEmpty {
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
        // Default voice (runs inside actor)
        audioData = try await engines.synthesize(text: body.text)
    }

    var headers = HTTPHeaders()
    headers.add(name: .contentType, value: "audio/wav")
    headers.add(name: "X-Audio-Sample-Rate", value: "24000")
    return Response(status: .ok, headers: headers, body: .init(data: audioData))
}

private func handleListVoices() -> VoicesListResponse {
    let voicesDir = URL(fileURLWithPath: "voices")
    var voices: [VoiceInfo] = []

    if FileManager.default.fileExists(atPath: voicesDir.path) {
        if let contents = try? FileManager.default.contentsOfDirectory(
            at: voicesDir, includingPropertiesForKeys: nil
        ) {
            for file in contents where file.pathExtension == "wav" {
                voices.append(VoiceInfo(
                    id: file.deletingPathExtension().lastPathComponent,
                    name: file.deletingPathExtension().lastPathComponent,
                    description: nil
                ))
            }
        }
    }

    if !voices.contains(where: { $0.id == "default" }) {
        voices.insert(VoiceInfo(
            id: "default", name: "default", description: "PocketTTS default voice"
        ), at: 0)
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
    var voice_ref: String?
    var speed: Double?
    var format: String?
}

struct VoicesListResponse: Content {
    let voices: [VoiceInfo]
}

struct VoiceInfo: Content {
    let id: String
    let name: String?
    let description: String?
}
