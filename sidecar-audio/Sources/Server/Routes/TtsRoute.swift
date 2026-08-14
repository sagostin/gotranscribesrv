import FluidAudio
import Vapor

/// TTS routes:
/// - POST /synthesize — Text-to-speech synthesis (?backend=pocket|kokoro, default pocket)
/// - POST /synthesize/stream — Streaming TTS (PocketTTS, chunked L16 24kHz frames)
/// - POST /clone-voice — Extract voice embedding from audio (PocketTTS only)
/// - GET /voices — List available voice presets (system + filesystem + Kokoro)
func ttsRoutes(_ app: Application, engines: EngineManager) {
    app.post("synthesize") { req async throws -> Response in
        try await handleSynthesize(req: req, engines: engines)
    }

    app.post("synthesize", "stream") { req async throws -> Response in
        try await handleSynthesizeStream(req: req, engines: engines)
    }

    app.post("clone-voice") { req async throws -> Response in
        try await handleCloneVoice(req: req, engines: engines)
    }

    app.get("voices") { req async throws -> VoicesListResponse in
        try await handleListVoices(engines: engines)
    }
}

/// Synthesize a complete WAV. Backend is selected by:
///   1. `?backend=` query param (explicit)
///   2. `SIDECAR_TTS_DEFAULT_BACKEND` env (default; "pocket" if unset)
///
/// Voice cloning (`voice_data` / `voice_ref`) is **only** supported by the
/// pocket backend — requests that combine cloning with `backend=kokoro`
/// (or the kokoro default) return 422 with a clear message.
private func handleSynthesize(req: Request, engines: EngineManager) async throws -> Response {
    let body = try req.content.decode(SynthesizeRequest.self)
    let defaultBack = await engines.getTtsDefaultBackend()
    let explicitBackend = req.query[String.self, at: "backend"]
    let backend = (explicitBackend ?? defaultBack).lowercased()

    // Validate backend
    guard backend == "pocket" || backend == "kokoro" else {
        throw Abort(
            .badRequest,
            reason: "Unknown backend=\(backend). Valid values: pocket, kokoro."
        )
    }

    // Voice cloning requires pocket — Kokoro has no embedding/cloning pipeline.
    let wantsCloning = (body.voice_data?.isEmpty == false) || (body.voice_ref?.isEmpty == false)
    if wantsCloning && backend != "pocket" {
        throw Abort(
            .unprocessableEntity,
            reason: "voice cloning (voice_data/voice_ref) is only supported by the pocket backend. Re-send with ?backend=pocket or omit the cloning fields."
        )
    }

    // Resolve the voice against the per-backend catalogs. Maps nil/""/"default"
    // to the backend's built-in default (prevents a bogus HuggingFace fetch of
    // "default.safetensors"), auto-reroutes voices that belong to the other
    // backend when ?backend= wasn't explicit, and 422s unknown voices.
    let resolved = try VoiceResolver.resolve(
        voice: body.voice,
        backend: backend,
        backendExplicit: explicitBackend != nil,
        kokoroLoaded: await engines.hasKokoro(),
        logger: req.logger
    )

    let audioData: Data

    do {
        switch resolved.backend {
        case "kokoro":
            // Kokoro has no voice-cloning pipeline — text + voice ID only.
            audioData = try await engines.synthesizeKokoro(text: body.text, voice: resolved.voice)
        default: // "pocket"
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
                audioData = try await engines.synthesize(text: body.text, voice: resolved.voice)
            }
        }
    } catch let abort as AbortError {
        throw abort
    } catch let error as AssetDownloader.Error {
        // e.g. a voice safetensors 404 — treat as a client-fixable voice
        // problem rather than a generic 500 so the gateway doesn't mask it
        // as "TTS service unavailable".
        throw Abort(.unprocessableEntity, reason: "voice asset unavailable: \(error.localizedDescription)")
    } catch let error as PocketTTSError {
        throw Abort(.unprocessableEntity, reason: "PocketTTS failed: \(error.localizedDescription)")
    } catch let error as KokoroAneError {
        throw Abort(.unprocessableEntity, reason: "Kokoro TTS failed: \(error.localizedDescription)")
    }

    var headers = HTTPHeaders()
    headers.add(name: .contentType, value: "audio/wav")
    headers.add(name: "X-Audio-Sample-Rate", value: "24000")
    headers.add(name: "X-TTS-Backend", value: resolved.backend)
    headers.add(name: "X-TTS-Voice", value: resolved.effectiveVoice)
    return Response(status: .ok, headers: headers, body: .init(data: audioData))
}

/// Streaming TTS — yields 80ms Int16 L16 frames as they're generated.
/// Body: { "text": "...", "voice": "alba" }.
/// Optional `?backend=` query param — only `pocket` is accepted. Kokoro
/// does not expose a streaming API in FluidAudio 0.15.5, so streaming is
/// permanently locked to PocketTTS until upstream support lands.
private func handleSynthesizeStream(req: Request, engines: EngineManager) async throws -> Response {
    let body = try req.content.decode(SynthesizeRequest.self)
    let backend = (req.query[String.self, at: "backend"] ?? "pocket").lowercased()
    guard backend == "pocket" else {
        throw Abort(
            .notImplemented,
            reason: "backend=\(backend) does not support streaming — only pocket (PocketTTS) streams. Use POST /synthesize?backend=\(backend) for batch synthesis."
        )
    }
    // Same voice resolution as batch /synthesize — "default"/"" must map to
    // the PocketTTS built-in default, not a literal HuggingFace fetch.
    let resolved = try VoiceResolver.resolve(
        voice: body.voice,
        backend: "pocket",
        backendExplicit: false,
        kokoroLoaded: await engines.hasKokoro(),
        logger: req.logger
    )
    guard resolved.backend == "pocket" else {
        throw Abort(
            .notImplemented,
            reason: "voice \"\(resolved.effectiveVoice)\" belongs to the kokoro backend, which does not support streaming. Use POST /synthesize?backend=kokoro for batch synthesis."
        )
    }
    let stream = try await engines.pocketSynthesizeStream(text: body.text, voice: resolved.voice)

    var headers = HTTPHeaders()
    headers.add(name: .contentType, value: "audio/L16; rate=24000; channels=1")
    headers.add(name: "X-Audio-Sample-Rate", value: "24000")
    headers.add(name: "X-TTS-Backend", value: "pocket")
    headers.add(name: "X-TTS-Voice", value: resolved.effectiveVoice)

    // Convert the AsyncThrowingStream<AudioFrame> into a streaming Response.
    // managedAsyncStream auto-closes the stream on return (no .end required).
    let response = Response(status: .ok, headers: headers)
    response.body = .init(managedAsyncStream: { writer in
        for try await frame in stream {
            // AudioFrame.samples is [Float] — convert to Int16 little-endian
            let pcm = frame.samples.map { sample -> Int16 in
                let clamped = max(-1.0, min(1.0, sample))
                return Int16(clamped * 32767)
            }
            var bytes = Data(capacity: pcm.count * 2)
            for s in pcm {
                withUnsafeBytes(of: s.littleEndian) { bytes.append(contentsOf: $0) }
            }
            let buffer = ByteBufferAllocator().buffer(bytes: bytes)
            try await writer.writeBuffer(buffer)
        }
    })
    return response
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

private func handleListVoices(engines: EngineManager) async throws -> VoicesListResponse {
    // PocketTTS built-in voices (back-compat with original /voices response).
    // "default" stays listed — VoiceResolver maps it to the backend default.
    var voices: [VoiceInfo] = [
        VoiceInfo(id: "default", name: "default", description: "PocketTTS default voice", type: "system", backend: "pocket")
    ]
    for v in VoiceResolver.pocketVoiceCatalog {
        voices.append(VoiceInfo(id: v.id, name: v.name, description: v.description, type: "system", backend: "pocket"))
    }
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
                        type: "system",
                        backend: "pocket"
                    ))
                }
            }
        }
    }

    // Kokoro voice catalog — only include the voices if Kokoro loaded successfully.
    if await engines.hasKokoro() {
        for v in VoiceResolver.kokoroVoiceCatalog {
            // Kokoro voice IDs include variant prefix (en-/zh-/ja-) — keep raw.
            voices.append(VoiceInfo(
                id: v.id, name: v.name, description: v.description,
                type: "system", backend: "kokoro"
            ))
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
    var backend: String? = nil  // "pocket" (default) or "kokoro"
}
