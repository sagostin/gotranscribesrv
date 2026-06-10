import FluidAudio
import ITNHelpers
import Vapor

/// Real-time streaming ASR route — WS /stream
///
/// Protocol:
///   Client → Server: Binary PCM 16-bit 16kHz mono frames + JSON control
///   Server → Client: JSON events (ready, partial, final, done, error)
///
/// Supports audio encodings via query params:
///   ?encoding=mulaw&sample_rate=8000   (G.711 μ-law, common telephony)
///   ?encoding=alaw&sample_rate=8000    (G.711 A-law)
///   ?encoding=linear16&sample_rate=16000 (default, raw PCM)
func streamRoutes(_ app: Application, engines: EngineManager) {
    app.webSocket("stream") { req, ws async in
        await handleStream(req: req, ws: ws, engines: engines)
    }
}

/// Minimum audio samples (at 16kHz) before attempting transcription (~1 second)
private let minTranscribeSamples = 16_000

/// Interval between partial transcriptions (in 16kHz samples, ~2 seconds)
private let partialIntervalSamples = 32_000

/// Audio encoding from query params
private enum AudioEncoding: String {
    case linear16
    case mulaw
    case alaw
}

/// Thread-safe state for a streaming session.
private final class StreamState: @unchecked Sendable {
    let encoding: AudioEncoding
    let inputSampleRate: Int
    let itnEnabled: Bool

    private let lock = NSLock()
    private var _pcmBuffer: [Float] = []   // Decoded + resampled to 16kHz Float32
    private var _lastTranscribedCount = 0
    private var _sessionClosed = false

    init(encoding: AudioEncoding, inputSampleRate: Int, itnEnabled: Bool) {
        self.encoding = encoding
        self.inputSampleRate = inputSampleRate
        self.itnEnabled = itnEnabled
    }

    var pcmBuffer: [Float] {
        lock.lock()
        defer { lock.unlock() }
        return _pcmBuffer
    }

    var sessionClosed: Bool {
        lock.lock()
        defer { lock.unlock() }
        return _sessionClosed
    }

    func appendSamples(_ samples: [Float]) {
        lock.lock()
        _pcmBuffer.append(contentsOf: samples)
        lock.unlock()
    }

    func markTranscribed() {
        lock.lock()
        _lastTranscribedCount = _pcmBuffer.count
        lock.unlock()
    }

    func close() {
        lock.lock()
        _sessionClosed = true
        lock.unlock()
    }

    var sampleCount: Int {
        lock.lock()
        defer { lock.unlock() }
        return _pcmBuffer.count
    }

    var newSampleCount: Int {
        lock.lock()
        defer { lock.unlock() }
        return _pcmBuffer.count - _lastTranscribedCount
    }
}

private func handleStream(req: Request, ws: WebSocket, engines: EngineManager) async {
    // Parse audio format from query params
    let encodingStr = req.query[String.self, at: "encoding"] ?? "linear16"
    let sampleRateStr = req.query[String.self, at: "sample_rate"] ?? "16000"
    let encoding = AudioEncoding(rawValue: encodingStr) ?? .linear16
    let inputSampleRate = Int(sampleRateStr) ?? 16000

    // ITN: per-session opt-out via ?itn=false. Default is on.
    let itnEnabled = (req.query[String.self, at: "itn"] ?? "true").lowercased() != "false"

    req.logger.info("[STREAM] Session started — encoding=\(encoding.rawValue), sample_rate=\(inputSampleRate), itn=\(itnEnabled)")

    let state = StreamState(encoding: encoding, inputSampleRate: inputSampleRate, itnEnabled: itnEnabled)

    // Send ready event
    try? await ws.send(#"{"type":"ready"}"#)
    req.logger.info("[STREAM] Sent ready event, waiting for audio frames...")

    // Handle incoming binary audio frames
    ws.onBinary { ws, buffer async in
        guard !state.sessionClosed else { return }

        var buf = buffer
        let byteCount = buf.readableBytes

        guard byteCount > 0 else {
            req.logger.warning("[STREAM] Empty binary frame, skipping")
            return
        }

        guard let bytes = buf.readBytes(length: byteCount) else {
            req.logger.warning("[STREAM] Failed to read bytes from buffer")
            return
        }

        // Decode audio based on encoding
        let decodedSamples: [Float]
        switch state.encoding {
        case .mulaw:
            // G.711 μ-law → PCM Float32
            let pcm16 = bytes.map { mulawDecode($0) }
            decodedSamples = pcm16.map { Float($0) / 32768.0 }
        case .alaw:
            // G.711 A-law → PCM Float32
            let pcm16 = bytes.map { alawDecode($0) }
            decodedSamples = pcm16.map { Float($0) / 32768.0 }
        case .linear16:
            // Raw PCM 16-bit LE → Float32
            let sampleCount = byteCount / 2
            let int16Samples = bytes.withUnsafeBufferPointer { ptr -> [Int16] in
                ptr.baseAddress!.withMemoryRebound(to: Int16.self, capacity: sampleCount) { p in
                    Array(UnsafeBufferPointer(start: p, count: sampleCount))
                }
            }
            decodedSamples = int16Samples.map { Float($0) / 32768.0 }
        }

        // Resample to 16kHz if needed
        let samples16k: [Float]
        if state.inputSampleRate == 8000 {
            samples16k = upsample2x(decodedSamples)
        } else if state.inputSampleRate != 16000 {
            // Simple nearest-neighbor resampling for other rates
            let ratio = 16000.0 / Double(state.inputSampleRate)
            let outCount = Int(Double(decodedSamples.count) * ratio)
            samples16k = (0..<outCount).map { i in
                let srcIdx = Int(Double(i) / ratio)
                return decodedSamples[min(srcIdx, decodedSamples.count - 1)]
            }
        } else {
            samples16k = decodedSamples
        }

        state.appendSamples(samples16k)

        if state.sampleCount % 32000 < samples16k.count {
            req.logger.info("[STREAM] Buffer: \(state.sampleCount) samples (\(String(format: "%.1f", Double(state.sampleCount) / 16000.0))s), new: \(state.newSampleCount)")
        }

        // Check if we have enough new audio for a partial transcription
        let newCount = state.newSampleCount
        let totalCount = state.sampleCount
        if newCount >= partialIntervalSamples && totalCount >= minTranscribeSamples {
            req.logger.info("[STREAM] Triggering partial transcription (\(totalCount) samples, \(String(format: "%.1f", Double(totalCount) / 16000.0))s)")
            let floatSamples = state.pcmBuffer
            state.markTranscribed()

            do {
                let result = try await engines.transcribe(floatSamples)
                req.logger.info("[STREAM] ASR result: \"\(result.text.prefix(200))\", length=\(result.text.count)")
                if !result.text.isEmpty {
                    let itn = TextNormalizer.shared
                    let outText: String
                    if state.itnEnabled {
                        // Phone-number pre-pass routes digit runs through
                        // single-expression `normalize()` (telephone tagger).
                        // See ITNPreprocessor for the why.
                        let pre = ITNPreprocessor.preprocessPhoneNumbers(result.text, normalizer: itn)
                        outText = itn.normalizeSentence(pre)
                    } else {
                        outText = result.text
                    }
                    // ITN debug — always print to stdout for live transcript
                    // visibility, plus a debug log line when the text changed.
                    if state.itnEnabled {
                        let native = TextNormalizer.shared.isNativeAvailable ? "ne" : "swift-passthrough"
                        if outText != result.text {
                            print("─[ITN \(native) partial]─────────────────────")
                            print("  before: \"\(result.text)\"")
                            print("  after : \"\(outText)\"")
                            print("─────────────────────────────────────────────")
                            req.logger.debug("ITN [\(native)] partial: \"\(result.text)\" -> \"\(outText)\"")
                        }
                    }
                    let partial = streamEventJSON(type: "partial", text: outText, isFinal: false)
                    try? await ws.send(partial)
                }
            } catch {
                req.logger.warning("[STREAM] Partial transcription failed: \(error)")
            }
        }
    }

    // Handle control messages
    ws.onText { ws, text async in
        guard !state.sessionClosed else { return }

        req.logger.info("[STREAM] Received text frame: \(text.prefix(500))")

        if let data = text.data(using: .utf8),
           let ctrl = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
            if let action = ctrl["action"] as? String, action == "stop" {
                req.logger.info("[STREAM] Stop action, finalizing (\(state.sampleCount) samples)")
                state.close()
                await finalizeStream(ws: ws, state: state, engines: engines, logger: req.logger)
            }
            if let type = ctrl["type"] as? String {
                switch type {
                case "KeepAlive":
                    req.logger.info("[STREAM] KeepAlive")
                case "CloseStream":
                    req.logger.info("[STREAM] CloseStream, finalizing (\(state.sampleCount) samples)")
                    state.close()
                    await finalizeStream(ws: ws, state: state, engines: engines, logger: req.logger)
                default:
                    req.logger.info("[STREAM] Unknown control: \(type)")
                }
            }
        }
    }

    ws.onClose.whenComplete { _ in
        req.logger.info("[STREAM] Session closed (\(state.sampleCount) samples)")
    }
}

/// Perform final transcription and send results.
private func finalizeStream(
    ws: WebSocket, state: StreamState, engines: EngineManager, logger: Logger
) async {
    let floatSamples = state.pcmBuffer

    guard floatSamples.count >= minTranscribeSamples else {
        logger.info("[STREAM] Not enough audio (\(floatSamples.count) samples)")
        try? await ws.send(#"{"type":"done"}"#)
        try? await ws.close()
        return
    }

    let audioDuration = Double(floatSamples.count) / 16000.0

    do {
        let result = try await engines.transcribe(floatSamples)
        logger.info("[STREAM] Final ASR: \"\(result.text.prefix(200))\"")

        // Build words array from token timings (per-word ITN applies to the
        // surface text; cross-word digit grouping is applied to the joined
        // text + final result.text below).
        let itn = TextNormalizer.shared
        let itnEnabled = state.itnEnabled
        var words: [[String: Any]] = []
        if let timings = result.tokenTimings {
            var currentWord = ""
            var wordStart: Double = 0
            var wordEnd: Double = 0

            for t in timings {
                let token = t.token
                if token.isEmpty || token == "<blank>" || token == "<pad>" { continue }

                let isWordStart = token.hasPrefix("▁") || token.hasPrefix(" ")
                let cleaned: String
                if isWordStart {
                    cleaned = String(token.dropFirst()).trimmingCharacters(in: .whitespaces)
                } else {
                    cleaned = token.trimmingCharacters(in: .whitespaces)
                }
                guard !cleaned.isEmpty else { continue }

                if isWordStart && !currentWord.isEmpty {
                    let surface = itnEnabled ? itn.normalize(currentWord) : currentWord
                    words.append(["word": surface, "start": wordStart, "end": wordEnd])
                    currentWord = cleaned
                    wordStart = t.startTime
                    wordEnd = t.endTime
                } else if isWordStart || currentWord.isEmpty {
                    currentWord = cleaned
                    wordStart = t.startTime
                    wordEnd = t.endTime
                } else {
                    currentWord += cleaned
                    wordEnd = t.endTime
                }
            }
            if !currentWord.isEmpty {
                let surface = itnEnabled ? itn.normalize(currentWord) : currentWord
                words.append(["word": surface, "start": wordStart, "end": wordEnd])
            }
        }

        let finalText: String
        if itnEnabled {
            let pre = ITNPreprocessor.preprocessPhoneNumbers(result.text, normalizer: itn)
            finalText = itn.normalizeSentence(pre)
        } else {
            finalText = result.text
        }

        // ITN debug — final event is rare, log at info even when unchanged
        // so operators can see the lib status on every session.
        if itnEnabled {
            let nativeLoaded = itn.isNativeAvailable ? "ne" : "swift-passthrough"
            print("─[ITN \(nativeLoaded) final]────────────────────────")
            print("  before: \"\(result.text)\"")
            print("  after : \"\(finalText)\"")
            print("─────────────────────────────────────────────────")
            if finalText != result.text {
                logger.info("ITN [\(nativeLoaded)] final: \"\(result.text)\" -> \"\(finalText)\"")
            } else {
                logger.info("ITN [\(nativeLoaded)] final: unchanged (\"\(result.text)\")")
            }
        } else {
            print("─[ITN]──────────────────────────────────────────")
            print("  disabled for this session (?itn=false)")
            print("  before: \"\(result.text)\"")
            print("  after : \"\(result.text)\"  (unchanged)")
            print("─────────────────────────────────────────────────")
            logger.info("ITN: disabled for this session (?itn=false)")
        }

        let finalEvent: [String: Any] = [
            "type": "final",
            "text": finalText,
            "start": 0.0,
            "end": audioDuration,
            "words": words,
            "is_final": true,
            "itn_applied": itnEnabled
        ]
        if let data = try? JSONSerialization.data(withJSONObject: finalEvent),
           let json = String(data: data, encoding: .utf8) {
            try? await ws.send(json)
        }
    } catch {
        logger.error("[STREAM] Final transcription failed: \(error)")
        try? await ws.send(#"{"type":"error","message":"transcription failed"}"#)
    }

    try? await ws.send(#"{"type":"done"}"#)
    try? await ws.close()
}

// MARK: - Audio Codec Helpers

/// G.711 μ-law decode table (ITU-T G.711)
private func mulawDecode(_ ulaw: UInt8) -> Int16 {
    // Invert all bits
    let u = ~ulaw
    let sign: Int16 = (u & 0x80) != 0 ? -1 : 1
    let exponent = Int((u >> 4) & 0x07)
    let mantissa = Int(u & 0x0F)

    var magnitude = (mantissa << 4) + 8
    if exponent > 0 {
        magnitude += 0x100
        if exponent > 1 {
            magnitude <<= (exponent - 1)
        }
    }

    return Int16(clamping: sign * Int16(clamping: magnitude))
}

/// G.711 A-law decode table (ITU-T G.711)
private func alawDecode(_ alaw: UInt8) -> Int16 {
    let a = alaw ^ 0x55 // Toggle even bits
    let sign: Int16 = (a & 0x80) != 0 ? -1 : 1
    let exponent = Int((a >> 4) & 0x07)
    let mantissa = Int(a & 0x0F)

    var magnitude: Int
    if exponent == 0 {
        magnitude = (mantissa << 4) + 8
    } else {
        magnitude = ((mantissa << 4) + 0x108) << (exponent - 1)
    }

    return Int16(clamping: sign * Int16(clamping: magnitude))
}

/// Simple 2x upsampling (8kHz → 16kHz) via linear interpolation.
private func upsample2x(_ input: [Float]) -> [Float] {
    guard input.count > 1 else { return input + input }
    var output = [Float](repeating: 0, count: input.count * 2)
    for i in 0..<input.count {
        output[i * 2] = input[i]
        if i + 1 < input.count {
            output[i * 2 + 1] = (input[i] + input[i + 1]) / 2.0
        } else {
            output[i * 2 + 1] = input[i]
        }
    }
    return output
}

// MARK: - Event Helpers

private func streamEventJSON(type: String, text: String, isFinal: Bool) -> String {
    let dict: [String: Any] = ["type": type, "text": text, "is_final": isFinal]
    if let data = try? JSONSerialization.data(withJSONObject: dict),
       let json = String(data: data, encoding: .utf8) {
        return json
    }
    return #"{"type":"error","message":"json encoding failed"}"#
}
