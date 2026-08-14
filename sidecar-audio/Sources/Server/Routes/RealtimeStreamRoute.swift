import AVFoundation
import FluidAudio
import ITNHelpers
import Vapor

/// True real-time streaming ASR route — WS /stream/realtime
///
/// Unlike the legacy `/stream` route (which buffers all audio and
/// re-transcribes the full buffer every ~2s with the offline TDT model),
/// this route uses FluidAudio's cache-aware streaming engines:
///   - Parakeet EOU 120M   (eou-160 / eou-320 / eou-1280) — built-in
///     end-of-utterance detection → free turn-taking for voice agents
///   - Nemotron 0.6B       (nemotron-560 / nemotron-1120 / nemotron-2240)
///   - Parakeet Unified    (unified-320 / unified-640 / unified-1120 /
///     unified-2080) — TDT v3 0.6B quality in true streaming form
///
/// Streaming Silero VAD runs alongside the ASR engine and emits
/// speech_started / speech_stopped events for agent turn-taking.
///
/// Protocol:
///   Client → Server: Binary PCM frames + JSON control (stop / CloseStream / KeepAlive)
///   Server → Client: JSON events (ready, speech_started, partial, final,
///                    end_of_turn, speech_stopped, done, error)
///
/// Query params:
///   ?engine=eou-320          (default: env SIDECAR_REALTIME_ENGINE, fallback eou-320)
///   ?encoding=linear16|mulaw|alaw   (default linear16)
///   ?sample_rate=16000       (8kHz input is upsampled 2x)
///   ?itn=true|false          (default true)
///   ?vad=true|false          (default true — speech_started/speech_stopped events)
func realtimeStreamRoutes(_ app: Application, engines: EngineManager) {
    app.webSocket("stream", "realtime") { req, ws async in
        // Resolve per-connection default from the manager, which reads
        // SIDECAR_REALTIME_ENGINE once at startup via applyConfig().
        let defaultEngine = await engines.getRealtimeEngine()
        await handleRealtimeStream(req: req, ws: ws, engines: engines, defaultEngineRaw: defaultEngine)
    }
}

/// Map the ?engine= query param to a FluidAudio streaming model variant.
private func realtimeEngineVariant(_ raw: String) -> StreamingModelVariant? {
    switch raw {
    case "eou-160": return .parakeetEou160ms
    case "eou-320": return .parakeetEou320ms
    case "eou-1280": return .parakeetEou1280ms
    case "nemotron-560": return .nemotron560ms
    case "nemotron-1120": return .nemotron1120ms
    case "nemotron-2240": return .nemotron2240ms
    case "unified-320": return .parakeetUnified320ms
    case "unified-640": return .parakeetUnified640ms
    case "unified-1120": return .parakeetUnified1120ms
    case "unified-2080": return .parakeetUnified2080ms
    default: return nil
    }
}

/// Thread-safe state for a realtime streaming session.
private final class RealtimeStreamState: @unchecked Sendable {
    let encoding: StreamAudioEncoding
    let inputSampleRate: Int
    let itnEnabled: Bool
    let vadEnabled: Bool

    private let lock = NSLock()
    private var _sessionClosed = false
    private var _engineReady = false
    private var _pendingSamples: [Float] = []   // buffered until engine load completes
    private var _vadBacklog: [Float] = []       // drained in 4096-sample chunks
    private var _vadState = VadStreamState.initial()
    private var _samplesProcessed = 0

    init(encoding: StreamAudioEncoding, inputSampleRate: Int, itnEnabled: Bool, vadEnabled: Bool) {
        self.encoding = encoding
        self.inputSampleRate = inputSampleRate
        self.itnEnabled = itnEnabled
        self.vadEnabled = vadEnabled
    }

    var sessionClosed: Bool {
        lock.lock()
        defer { lock.unlock() }
        return _sessionClosed
    }

    var engineReady: Bool {
        lock.lock()
        defer { lock.unlock() }
        return _engineReady
    }

    var vadState: VadStreamState {
        get {
            lock.lock()
            defer { lock.unlock() }
            return _vadState
        }
        set {
            lock.lock()
            _vadState = newValue
            lock.unlock()
        }
    }

    var samplesProcessed: Int {
        lock.lock()
        defer { lock.unlock() }
        return _samplesProcessed
    }

    func close() {
        lock.lock()
        _sessionClosed = true
        lock.unlock()
    }

    func markEngineReady() {
        lock.lock()
        _engineReady = true
        lock.unlock()
    }

    /// Append samples; returns pending backlog if engine just became usable.
    func appendPending(_ samples: [Float]) {
        lock.lock()
        _pendingSamples.append(contentsOf: samples)
        lock.unlock()
    }

    func drainPending() -> [Float] {
        lock.lock()
        let pending = _pendingSamples
        _pendingSamples.removeAll()
        lock.unlock()
        return pending
    }

    func appendVadBacklog(_ samples: [Float]) {
        lock.lock()
        _vadBacklog.append(contentsOf: samples)
        lock.unlock()
    }

    /// Drain up to one 4096-sample VAD chunk, or nil if not enough buffered.
    func drainVadChunk() -> [Float]? {
        lock.lock()
        guard _vadBacklog.count >= VadManager.chunkSize else {
            lock.unlock()
            return nil
        }
        let chunk = Array(_vadBacklog.prefix(VadManager.chunkSize))
        _vadBacklog.removeFirst(VadManager.chunkSize)
        _samplesProcessed += chunk.count
        lock.unlock()
        return chunk
    }
}

/// Serializes outbound WebSocket sends (partial callbacks fire from the
/// streaming engine's actor context).
private actor RealtimeSender {
    private let ws: WebSocket

    init(ws: WebSocket) {
        self.ws = ws
    }

    func send(_ json: String) {
        Task { try? await ws.send(json) }
    }

    func sendJSON(_ dict: [String: Any]) {
        guard let data = try? JSONSerialization.data(withJSONObject: dict),
              let json = String(data: data, encoding: .utf8) else { return }
        send(json)
    }
}

/// Audio encodings (mirrors StreamRoute's private enum).
private enum StreamAudioEncoding: String {
    case linear16
    case mulaw
    case alaw
}

private func handleRealtimeStream(
    req: Request, ws: WebSocket, engines: EngineManager, defaultEngineRaw: String
) async {
    // Parse query params
    let encodingStr = req.query[String.self, at: "encoding"] ?? "linear16"
    let sampleRateStr = req.query[String.self, at: "sample_rate"] ?? "16000"
    let encoding = StreamAudioEncoding(rawValue: encodingStr) ?? .linear16
    let inputSampleRate = Int(sampleRateStr) ?? 16000
    let itnEnabled = (req.query[String.self, at: "itn"] ?? "true").lowercased() != "false"
    let vadEnabled = (req.query[String.self, at: "vad"] ?? "true").lowercased() != "false"
    let engineRaw = req.query[String.self, at: "engine"] ?? defaultEngineRaw

    guard let variant = realtimeEngineVariant(engineRaw) else {
        try? await ws.send(#"{"type":"error","message":"unknown engine"}"#)
        try? await ws.close()
        return
    }

    req.logger.info("[RT] Session started — engine=\(variant.rawValue), encoding=\(encoding.rawValue), sample_rate=\(inputSampleRate), itn=\(itnEnabled), vad=\(vadEnabled)")

    let state = RealtimeStreamState(
        encoding: encoding, inputSampleRate: inputSampleRate,
        itnEnabled: itnEnabled, vadEnabled: vadEnabled)
    let sender = RealtimeSender(ws: ws)

    // Create the streaming engine for this session.
    let engine = variant.createManager()

    // Partial transcripts → client
    await engine.setPartialTranscriptCallback { text in
        guard !text.isEmpty else { return }
        Task {
            await sender.sendJSON(["type": "partial", "text": text, "is_final": false])
        }
    }

    // EOU engines: built-in end-of-utterance → turn boundary events
    if let eouEngine = engine as? StreamingEouAsrManager {
        await eouEngine.setEouCallback { transcript in
            Task {
                await emitTurnEnd(
                    transcript: transcript, sender: sender, state: state, logger: req.logger)
            }
        }
    }

    // Incoming audio: queue until engine is loaded, then forward.
    ws.onBinary { ws, buffer async in
        guard !state.sessionClosed else { return }

        var buf = buffer
        let byteCount = buf.readableBytes
        guard byteCount > 0, let bytes = buf.readBytes(length: byteCount) else { return }

        let samples16k = decodeTo16k(bytes, encoding: state.encoding, inputSampleRate: state.inputSampleRate)
        guard !samples16k.isEmpty else { return }

        if !state.engineReady {
            state.appendPending(samples16k)
            return
        }

        await processRealtimeAudio(samples16k, engine: engine, engines: engines, state: state, sender: sender, logger: req.logger)
    }

    // Control messages
    ws.onText { ws, text async in
        guard !state.sessionClosed else { return }
        guard let data = text.data(using: .utf8),
              let ctrl = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return }

        let action = ctrl["action"] as? String
        let type = ctrl["type"] as? String
        if action == "stop" || type == "CloseStream" {
            req.logger.info("[RT] Finalizing session")
            state.close()
            await finalizeRealtime(ws: ws, engine: engine, state: state, sender: sender, logger: req.logger)
        }
    }

    ws.onClose.whenComplete { _ in
        req.logger.info("[RT] Session closed (\(state.samplesProcessed) samples processed)")
        state.close()
        Task { await engine.cleanup() }
    }

    // Load engine models (downloads on first use, compiled-cache hit after).
    do {
        try await engine.loadModels()
        state.markEngineReady()
        await sender.sendJSON([
            "type": "ready",
            "engine": variant.rawValue,
            "display_name": variant.displayName,
        ])
        req.logger.info("[RT] Engine loaded: \(variant.displayName)")

        // Flush audio buffered during model load
        let pending = state.drainPending()
        if !pending.isEmpty {
            await processRealtimeAudio(pending, engine: engine, engines: engines, state: state, sender: sender, logger: req.logger)
        }
    } catch {
        req.logger.error("[RT] Engine load failed: \(error)")
        await sender.sendJSON(["type": "error", "message": "engine load failed"])
        try? await ws.close()
    }
}

/// Decode one binary frame to 16kHz Float32 samples.
private func decodeTo16k(_ bytes: [UInt8], encoding: StreamAudioEncoding, inputSampleRate: Int) -> [Float] {
    let decoded: [Float]
    switch encoding {
    case .mulaw:
        decoded = bytes.map { Float(mulawDecode($0)) / 32768.0 }
    case .alaw:
        decoded = bytes.map { Float(alawDecode($0)) / 32768.0 }
    case .linear16:
        let sampleCount = bytes.count / 2
        let int16Samples = bytes.withUnsafeBufferPointer { ptr -> [Int16] in
            ptr.baseAddress!.withMemoryRebound(to: Int16.self, capacity: sampleCount) { p in
                Array(UnsafeBufferPointer(start: p, count: sampleCount))
            }
        }
        decoded = int16Samples.map { Float($0) / 32768.0 }
    }

    if inputSampleRate == 8000 {
        return upsample2x(decoded)
    }
    if inputSampleRate != 16000 {
        let ratio = 16000.0 / Double(inputSampleRate)
        let outCount = Int(Double(decoded.count) * ratio)
        return (0..<outCount).map { i in
            let srcIdx = Int(Double(i) / ratio)
            return decoded[min(srcIdx, decoded.count - 1)]
        }
    }
    return decoded
}

/// Feed decoded 16kHz samples to the streaming ASR engine and the VAD.
private func processRealtimeAudio(
    _ samples: [Float],
    engine: any StreamingAsrManager,
    engines: EngineManager,
    state: RealtimeStreamState,
    sender: RealtimeSender,
    logger: Logger
) async {
    // Streaming VAD — 256ms chunks with hysteresis events
    if state.vadEnabled {
        state.appendVadBacklog(samples)
        while let chunk = state.drainVadChunk() {
            do {
                let result = try await engines.vadStreamingChunk(chunk, state: state.vadState)
                state.vadState = result.state
                if let event = result.event {
                    let time = Double(event.sampleIndex) / 16000.0
                    if event.isStart {
                        logger.info("[RT] speech_started @ \(String(format: "%.2f", time))s")
                        await sender.sendJSON(["type": "speech_started", "time": time])
                    } else {
                        logger.info("[RT] speech_stopped @ \(String(format: "%.2f", time))s")
                        await sender.sendJSON(["type": "speech_stopped", "time": time])

                        // Non-EOU engines have no built-in endpointing — the
                        // VAD speech boundary marks the turn instead.
                        if !(engine is StreamingEouAsrManager) {
                            let transcript = await engine.getPartialTranscript()
                            if !transcript.isEmpty {
                                await emitTurnEnd(transcript: transcript, sender: sender, state: state, logger: logger)
                            }
                        }
                    }
                }
            } catch {
                logger.warning("[RT] VAD chunk failed: \(error)")
            }
        }
    }

    // Streaming ASR — cache-aware incremental inference
    guard let pcm = makePCMBuffer(samples) else { return }
    do {
        try await engine.appendAudio(pcm)
        try await engine.processBufferedAudio()
    } catch {
        logger.warning("[RT] Streaming ASR chunk failed: \(error)")
    }
}

/// Emit the end-of-turn final transcript (ITN applied).
private func emitTurnEnd(
    transcript: String,
    sender: RealtimeSender,
    state: RealtimeStreamState,
    logger: Logger
) async {
    let trimmed = transcript.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !trimmed.isEmpty else { return }

    let finalText: String
    if state.itnEnabled {
        let itn = TextNormalizer.shared
        let pre = ITNPreprocessor.preprocessPhoneNumbers(trimmed, normalizer: itn)
        finalText = itn.normalizeSentence(pre)
    } else {
        finalText = trimmed
    }

    logger.info("[RT] end_of_turn: \"\(finalText.prefix(200))\"")
    await sender.sendJSON([
        "type": "final",
        "text": finalText,
        "is_final": true,
        "speech_final": true,
        "itn_applied": state.itnEnabled,
    ])
    await sender.sendJSON(["type": "end_of_turn"])
}

/// Final transcription at end of session.
private func finalizeRealtime(
    ws: WebSocket,
    engine: any StreamingAsrManager,
    state: RealtimeStreamState,
    sender: RealtimeSender,
    logger: Logger
) async {
    do {
        let text = try await engine.finish()
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        if !trimmed.isEmpty {
            let finalText: String
            if state.itnEnabled {
                let itn = TextNormalizer.shared
                let pre = ITNPreprocessor.preprocessPhoneNumbers(trimmed, normalizer: itn)
                finalText = itn.normalizeSentence(pre)
            } else {
                finalText = trimmed
            }
            logger.info("[RT] Final: \"\(finalText.prefix(200))\"")
            await sender.sendJSON([
                "type": "final",
                "text": finalText,
                "is_final": true,
                "itn_applied": state.itnEnabled,
            ])
        }
    } catch {
        logger.error("[RT] Final transcription failed: \(error)")
        await sender.sendJSON(["type": "error", "message": "transcription failed"])
    }

    await sender.send(#"{"type":"done"}"#)
    try? await ws.close()
}

/// Build a 16kHz mono AVAudioPCMBuffer from Float32 samples.
private func makePCMBuffer(_ samples: [Float]) -> AVAudioPCMBuffer? {
    guard let format = AVAudioFormat(standardFormatWithSampleRate: 16000, channels: 1),
          let buffer = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: AVAudioFrameCount(samples.count))
    else { return nil }
    buffer.frameLength = AVAudioFrameCount(samples.count)
    samples.withUnsafeBufferPointer { ptr in
        if let base = ptr.baseAddress, let dst = buffer.floatChannelData?[0] {
            dst.update(from: base, count: samples.count)
        }
    }
    return buffer
}
