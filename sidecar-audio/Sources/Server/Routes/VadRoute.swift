import FluidAudio
import Vapor

/// VAD route — POST /vad
func vadRoutes(_ app: Application, engines: EngineManager) {
    app.on(.POST, "vad", body: .collect(maxSize: "100mb")) { req async throws -> VadResponse in
        try await handleVad(req: req, engines: engines)
    }
}

private func handleVad(req: Request, engines: EngineManager) async throws -> VadResponse {
    let upload = try req.content.decode(AudioOnlyUpload.self)
    let audioData = Data(buffer: upload.audio.data)

    guard !audioData.isEmpty else {
        throw Abort(.unprocessableEntity, reason: "Audio file is required (field: 'audio')")
    }

    let startTime = ContinuousClock.now

    let samples = try await SidecarAudioConverter.toPCM16kMono(audioData)

    var segConfig = VadSegmentationConfig.default
    segConfig.minSpeechDuration = 0.25
    segConfig.minSilenceDuration = 0.3

    // Run inside actor
    let speechSegments = try await engines.vadSegment(samples, config: segConfig)

    let elapsed = ContinuousClock.now - startTime
    let processingTimeMs = Int(elapsed.components.seconds * 1000)
        + Int(elapsed.components.attoseconds / 1_000_000_000_000_000)

    let segments = speechSegments.map { VadSegment(start: $0.startTime, end: $0.endTime) }
    let duration = Double(samples.count) / 16000.0

    return VadResponse(
        speech_segments: segments,
        duration: duration,
        processing_time_ms: processingTimeMs
    )
}

// MARK: - Models

struct AudioOnlyUpload: Content {
    var audio: File
}

struct VadResponse: Content {
    let speech_segments: [VadSegment]
    let duration: Double
    let processing_time_ms: Int
}

struct VadSegment: Content {
    let start: Double
    let end: Double
}
