import FluidAudio
import ITNHelpers
import Vapor

/// Real-time streaming ASR route — WS /stream
///
/// Protocol:
///   Client → Server: Binary PCM 16-bit 16kHz mono frames + JSON control
///   Server → Client: JSON events (ready, speech_started, partial, final,
///                    utterance_end, done, error)
///
/// Supports audio encodings via query params:
///   ?encoding=mulaw&sample_rate=8000   (G.711 μ-law, common telephony)
///   ?encoding=alaw&sample_rate=8000    (G.711 A-law)
///   ?encoding=linear16&sample_rate=16000 (default, raw PCM)
///
/// Endpointing (Deepgram-compatible):
///   ?endpointing=300   Milliseconds of silence after speech end before an
///                      automatic segment final is emitted. Default 300 —
///                      Deepgram's spec default is 10ms, which finalizes
///                      mid-phrase on real speech; 300ms is their own
///                      recommended practical value. "false"/"0" disables
///                      auto-finalize (finals then only on Finalize /
///                      CloseStream, the pre-endpointing behavior).
///
/// Streaming Silero VAD runs alongside the buffered ASR. When an endpoint
/// fires, the current segment (audio since the last final) is transcribed
/// and emitted as a final with speech_final=true, followed by an
/// utterance_end event. speech_started events fire on speech onset. The
/// Deepgram proxy gates delivery of speech_started / utterance_end behind
/// the client's vad_events / utterance_end_ms params.
func streamRoutes(_ app: Application, engines: EngineManager) {
    app.webSocket("stream") { req, ws async in
        await handleStream(req: req, ws: ws, engines: engines)
    }
}

/// Minimum audio samples (at 16kHz) before attempting a partial (~1 second)
private let minTranscribeSamples = 16_000

/// Interval between partial transcriptions (in 16kHz samples, ~2 seconds)
private let partialIntervalSamples = 32_000

/// Minimum segment length for a final transcription (~100ms). Below this a
/// CloseStream/Finalize still gets an (empty) final so clients are never
/// left waiting; an endpoint trigger just resets the segment.
private let minFinalSamples = 1_600

/// Default endpointing: 300ms of trailing silence (see route doc comment).
private let defaultEndpointingMs = 300

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
    /// Known limitation: /stream has no real diarizer. When diarize=true we
    /// tag every word with speaker 0 so the Deepgram `speaker` field exists.
    let diarizeEnabled: Bool
    /// Silence (in 16kHz samples) after speech end that triggers an
    /// automatic segment final. nil = endpointing disabled.
    let endpointingSamples: Int?

    private let lock = NSLock()
    private var _pcmBuffer: [Float] = []   // Decoded + resampled to 16kHz Float32
    private var _lastTranscribedCount = 0
    private var _sessionClosed = false

    // MARK: Segment / endpointing state
    /// Sample index the current (unfinalized) segment starts at. Finals
    /// cover [finalizedUpTo, buffer.count); partials transcribe the same
    /// range so results are stream-relative like Deepgram's.
    private var _finalizedUpTo = 0
    /// Sample index where the VAD last saw speech end, awaiting the
    /// endpointing window. nil when speech is active or no endpoint pending.
    private var _pendingSpeechEnd: Int?
    /// Bumped whenever the pending endpoint changes so stale endpoint
    /// watchdogs no-op.
    private var _endpointGeneration = 0
    /// True while a transcription (partial or final) is in flight — used
    /// to skip overlapping partials and defer endpoint finals.
    private var _transcribing = false

    // MARK: VAD state
    private var _vadBacklog: [Float] = []       // drained in 4096-sample chunks
    private var _vadState = VadStreamState.initial()
    private var _vadSamplesProcessed = 0

    init(encoding: AudioEncoding, inputSampleRate: Int, itnEnabled: Bool, diarizeEnabled: Bool, endpointingSamples: Int?) {
        self.encoding = encoding
        self.inputSampleRate = inputSampleRate
        self.itnEnabled = itnEnabled
        self.diarizeEnabled = diarizeEnabled
        self.endpointingSamples = endpointingSamples
    }

    var pcmBuffer: [Float] {
        lock.lock()
        defer { lock.unlock() }
        return _pcmBuffer
    }

    /// Samples in the current (unfinalized) segment.
    var segmentSamples: [Float] {
        lock.lock()
        defer { lock.unlock() }
        return Array(_pcmBuffer[_finalizedUpTo...])
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

    var segmentSampleCount: Int {
        lock.lock()
        defer { lock.unlock() }
        return _pcmBuffer.count - _finalizedUpTo
    }

    var finalizedUpTo: Int {
        lock.lock()
        defer { lock.unlock() }
        return _finalizedUpTo
    }

    /// Mark all currently-buffered audio as finalized (start of a new segment).
    func markFinalized() {
        lock.lock()
        _finalizedUpTo = _pcmBuffer.count
        lock.unlock()
    }

    // MARK: Endpoint bookkeeping

    /// Record a VAD speech-end; returns the watchdog generation for it.
    @discardableResult
    func markSpeechEnd(sampleIndex: Int) -> Int {
        lock.lock()
        _pendingSpeechEnd = sampleIndex
        _endpointGeneration += 1
        defer { lock.unlock() }
        return _endpointGeneration
    }

    /// Cancel a pending endpoint (speech restarted, or a final consumed it).
    func clearPendingEndpoint() {
        lock.lock()
        _pendingSpeechEnd = nil
        _endpointGeneration += 1
        lock.unlock()
    }

    /// Sample index of the pending speech end, if any.
    var pendingSpeechEnd: Int? {
        lock.lock()
        defer { lock.unlock() }
        return _pendingSpeechEnd
    }

    /// Returns the pending endpoint if its silence window has elapsed.
    func endpointDue() -> Int? {
        lock.lock()
        defer { lock.unlock() }
        guard let end = _pendingSpeechEnd, let threshold = endpointingSamples else { return nil }
        return _vadSamplesProcessed - end >= threshold ? end : nil
    }

    /// True if the given watchdog generation is still current with a
    /// pending endpoint.
    func endpointPending(generation: Int) -> Bool {
        lock.lock()
        defer { lock.unlock() }
        return _endpointGeneration == generation && _pendingSpeechEnd != nil
    }

    // MARK: Transcription overlap guard

    func tryBeginTranscribe() -> Bool {
        lock.lock()
        defer { lock.unlock() }
        if _transcribing { return false }
        _transcribing = true
        return true
    }

    func endTranscribe() {
        lock.lock()
        _transcribing = false
        lock.unlock()
    }

    // MARK: VAD backlog

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
        _vadSamplesProcessed += chunk.count
        lock.unlock()
        return chunk
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
}

/// Parse the ?endpointing= query param. Returns nil when disabled.
private func parseEndpointing(_ req: Request) -> Int? {
    guard let raw = req.query[String.self, at: "endpointing"] else {
        return defaultEndpointingMs * 16 // default on, 300ms
    }
    switch raw.lowercased() {
    case "false", "0", "off", "disabled":
        return nil
    case "true", "":
        return defaultEndpointingMs * 16
    default:
        guard let ms = Int(raw), ms > 0 else { return defaultEndpointingMs * 16 }
        return ms * 16 // 16 samples per ms at 16kHz
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

    let endpointingSamples = parseEndpointing(req)
    let diarizeEnabled = (req.query[String.self, at: "diarize"] ?? "false").lowercased() == "true"

    req.logger.info("[STREAM] Session started — encoding=\(encoding.rawValue), sample_rate=\(inputSampleRate), itn=\(itnEnabled), diarize=\(diarizeEnabled), endpointing=\(endpointingSamples.map { "\($0 / 16)ms" } ?? "off")")

    let state = StreamState(encoding: encoding, inputSampleRate: inputSampleRate, itnEnabled: itnEnabled, diarizeEnabled: diarizeEnabled, endpointingSamples: endpointingSamples)

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

        // Streaming VAD → speech_started / endpoint detection. Runs before
        // the partial check so an endpoint final isn't delayed behind one.
        await processVad(samples16k, ws: ws, state: state, engines: engines, logger: req.logger)

        // Check if we have enough new audio for a partial transcription
        let newCount = state.newSampleCount
        let segmentCount = state.segmentSampleCount
        if newCount >= partialIntervalSamples && segmentCount >= minTranscribeSamples {
            // Skip if a transcription is already in flight (e.g. an
            // endpoint final) — the next interval will catch up.
            guard state.tryBeginTranscribe() else { return }
            req.logger.info("[STREAM] Triggering partial transcription (\(state.sampleCount) samples, \(String(format: "%.1f", Double(state.sampleCount) / 16000.0))s)")
            let segment = state.segmentSamples
            let segmentStart = state.finalizedUpTo
            state.markTranscribed()

            do {
                let result = try await engines.transcribe(segment)
                state.endTranscribe()
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
                    let startSec = Double(segmentStart) / 16000.0
                    let endSec = Double(segmentStart + segment.count) / 16000.0
                    let partial = streamEventJSON(type: "partial", text: outText, isFinal: false, start: startSec, end: endSec)
                    try? await ws.send(partial)
                }
            } catch {
                state.endTranscribe()
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
                case "Finalize":
                    // Deepgram-compat: flush the current buffer WITHOUT closing
                    // the session. Emits a final event tagged from_finalize so
                    // the proxy can mark the Results accordingly.
                    req.logger.info("[STREAM] Finalize, flushing (\(state.sampleCount) samples)")
                    // speech_final=true when the VAD already saw speech end
                    // (the Finalize coincided with an endpoint).
                    let speechFinal = state.pendingSpeechEnd != nil
                    await emitFinalEvent(ws: ws, state: state, engines: engines, logger: req.logger, fromFinalize: true, speechFinal: speechFinal)
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

/// Feed decoded 16kHz samples to the streaming VAD and emit
/// speech_started / endpoint finals as events arrive.
private func processVad(
    _ samples: [Float],
    ws: WebSocket,
    state: StreamState,
    engines: EngineManager,
    logger: Logger
) async {
    state.appendVadBacklog(samples)
    while let chunk = state.drainVadChunk() {
        do {
            let result = try await engines.vadStreamingChunk(chunk, state: state.vadState)
            state.vadState = result.state
            if let event = result.event {
                let time = Double(event.sampleIndex) / 16000.0
                if event.isStart {
                    logger.info("[STREAM] speech_started @ \(String(format: "%.2f", time))s")
                    state.clearPendingEndpoint()
                    try? await ws.send(#"{"type":"speech_started","timestamp":"# + "\(time)}")
                } else {
                    logger.info("[STREAM] speech_ended @ \(String(format: "%.2f", time))s")
                    let generation = state.markSpeechEnd(sampleIndex: event.sampleIndex)
                    // Watchdog: if the client stops sending audio right at
                    // the endpoint, no further onBinary calls arrive to
                    // notice the elapsed silence — fire it on a timer too.
                    scheduleEndpointWatchdog(ws: ws, state: state, engines: engines, logger: logger, generation: generation)
                }
            }
        } catch {
            logger.warning("[STREAM] VAD chunk failed: \(error)")
        }

        // Endpoint check after each drained chunk (256ms granularity).
        if state.endpointDue() != nil {
            await emitEndpointFinal(ws: ws, state: state, engines: engines, logger: logger)
        }
    }
}

/// Retry loop that fires a pending endpoint even when no further audio
/// frames arrive. The endpoint is due endpointing-ms after the speech end
/// was DETECTED (wall clock) — the VAD sample counter can stall when the
/// client stops sending, so the watchdog deliberately does not depend on
/// it. No-ops if the endpoint was already consumed/canceled.
private func scheduleEndpointWatchdog(
    ws: WebSocket,
    state: StreamState,
    engines: EngineManager,
    logger: Logger,
    generation: Int
) {
    guard let threshold = state.endpointingSamples else { return }
    let waitNs = UInt64(threshold / 16 + 300) * 1_000_000 // endpointing + VAD chunk slack
    Task {
        for _ in 0..<10 {
            try? await Task.sleep(nanoseconds: waitNs)
            guard !state.sessionClosed, state.endpointPending(generation: generation) else { return }
            // emitEndpointFinal returns false when another transcription is
            // in flight — the endpoint stays pending and we retry.
            if await emitEndpointFinal(ws: ws, state: state, engines: engines, logger: logger) {
                return
            }
        }
    }
}

/// Emit the segment final for a detected endpoint: transcribe the current
/// segment, send final (speech_final=true) + utterance_end, start a new
/// segment. Returns false when another transcription is in flight (the
/// endpoint stays pending for the next VAD chunk or watchdog retry).
@discardableResult
private func emitEndpointFinal(
    ws: WebSocket,
    state: StreamState,
    engines: EngineManager,
    logger: Logger
) async -> Bool {
    guard let speechEnd = state.pendingSpeechEnd else { return true }
    guard state.tryBeginTranscribe() else { return false }
    state.clearPendingEndpoint()
    defer { state.endTranscribe() }

    let segment = state.segmentSamples
    let segmentStart = state.finalizedUpTo
    let lastWordEnd = Double(speechEnd) / 16000.0

    // False VAD trigger on a tiny blip — just reset the segment.
    guard segment.count >= minFinalSamples else {
        state.markFinalized()
        return true
    }

    logger.info("[STREAM] Endpoint detected — finalizing segment (\(segment.count) samples, \(String(format: "%.1f", Double(segment.count) / 16000.0))s)")

    let result = await transcribeSegment(segment, state: state, engines: engines, logger: logger, label: "endpoint")
    if let result, !result.text.isEmpty {
        await sendFinal(ws: ws, state: state, result: result, segmentStart: segmentStart, segmentSampleCount: segment.count, fromFinalize: false, speechFinal: true)
    }
    state.markFinalized()
    try? await ws.send(#"{"type":"utterance_end","last_word_end":"# + "\(lastWordEnd)}")
    return true
}

/// Perform final transcription, send results, then signal done and close.
private func finalizeStream(
    ws: WebSocket, state: StreamState, engines: EngineManager, logger: Logger
) async {
    await emitFinalEvent(ws: ws, state: state, engines: engines, logger: logger, fromFinalize: false, speechFinal: true)

    try? await ws.send(#"{"type":"done"}"#)
    try? await ws.close()
}

/// Transcribe the current (unfinalized) segment and emit a single `final`
/// event. Used by CloseStream (fromFinalize=false, then done+close),
/// Finalize (fromFinalize=true, session stays open), and endpoint finals.
///
/// Always emits a final — even when the remaining segment is too short to
/// transcribe (empty text) — so a client that closes after <1s of audio
/// still gets a terminal result, matching Deepgram.
private func emitFinalEvent(
    ws: WebSocket, state: StreamState, engines: EngineManager, logger: Logger,
    fromFinalize: Bool, speechFinal: Bool
) async {
    state.clearPendingEndpoint()
    let segment = state.segmentSamples
    let segmentStart = state.finalizedUpTo

    guard segment.count >= minFinalSamples else {
        logger.info("[STREAM] Not enough audio (\(segment.count) samples), sending empty final")
        await sendFinal(ws: ws, state: state, result: nil, segmentStart: segmentStart, segmentSampleCount: segment.count, fromFinalize: fromFinalize, speechFinal: speechFinal)
        state.markFinalized()
        return
    }

    let result = await transcribeSegment(segment, state: state, engines: engines, logger: logger, label: "final")
    guard let result else {
        // transcribeSegment already logged; notify the client
        try? await ws.send(#"{"type":"error","message":"transcription failed"}"#)
        return
    }
    await sendFinal(ws: ws, state: state, result: result, segmentStart: segmentStart, segmentSampleCount: segment.count, fromFinalize: fromFinalize, speechFinal: speechFinal)
    state.markFinalized()
}

/// Transcribe a segment with ITN applied. Returns nil on failure.
private func transcribeSegment(
    _ segment: [Float],
    state: StreamState,
    engines: EngineManager,
    logger: Logger,
    label: String
) async -> (text: String, tokenTimings: [TokenTiming]?)? {
    do {
        let result = try await engines.transcribe(segment)
        logger.info("[STREAM] \(label.capitalized) ASR: \"\(result.text.prefix(200))\"")

        let finalText: String
        if state.itnEnabled {
            let itn = TextNormalizer.shared
            let pre = ITNPreprocessor.preprocessPhoneNumbers(result.text, normalizer: itn)
            finalText = itn.normalizeSentence(pre)
        } else {
            finalText = result.text
        }

        // ITN debug — final events are rare, log at info even when unchanged
        // so operators can see the lib status on every session.
        let itn = TextNormalizer.shared
        if state.itnEnabled {
            let nativeLoaded = itn.isNativeAvailable ? "ne" : "swift-passthrough"
            print("─[ITN \(nativeLoaded) \(label)]────────────────────────")
            print("  before: \"\(result.text)\"")
            print("  after : \"\(finalText)\"")
            print("─────────────────────────────────────────────────")
            if finalText != result.text {
                logger.info("ITN [\(nativeLoaded)] \(label): \"\(result.text)\" -> \"\(finalText)\"")
            } else {
                logger.info("ITN [\(nativeLoaded)] \(label): unchanged (\"\(result.text)\")")
            }
        } else {
            print("─[ITN]──────────────────────────────────────────")
            print("  disabled for this session (?itn=false)")
            print("  before: \"\(result.text)\"")
            print("  after : \"\(result.text)\"  (unchanged)")
            print("─────────────────────────────────────────────────")
            logger.info("ITN: disabled for this session (?itn=false)")
        }
        return (finalText, result.tokenTimings)
    } catch {
        logger.error("[STREAM] \(label.capitalized) transcription failed: \(error)")
        return nil
    }
}

/// Build and send a `final` event for a transcribed segment. Word timings
/// are built from token timings and offset to stream-relative seconds
/// (Deepgram semantics). A nil result produces the empty final used to
/// answer Finalize/CloseStream on too-short audio.
private func sendFinal(
    ws: WebSocket,
    state: StreamState,
    result: (text: String, tokenTimings: [TokenTiming]?)?,
    segmentStart: Int,
    segmentSampleCount: Int,
    fromFinalize: Bool,
    speechFinal: Bool
) async {
    let startSec = Double(segmentStart) / 16000.0
    let endSec = Double(segmentStart + segmentSampleCount) / 16000.0

    // Build words array from token timings (per-word ITN applies to the
    // surface text). Token times are segment-relative → offset to
    // stream-relative.
    var words: [[String: Any]] = []
    if let timings = result?.tokenTimings {
        let itn = TextNormalizer.shared
        let itnEnabled = state.itnEnabled
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
                var w: [String: Any] = ["word": surface, "start": wordStart + startSec, "end": wordEnd + startSec]
                if state.diarizeEnabled { w["speaker"] = "0" }
                words.append(w)
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
            var w: [String: Any] = ["word": surface, "start": wordStart + startSec, "end": wordEnd + startSec]
            if state.diarizeEnabled { w["speaker"] = "0" }
            words.append(w)
        }
    }

    let finalEvent: [String: Any] = [
        "type": "final",
        "text": result?.text ?? "",
        "start": startSec,
        "end": endSec,
        "words": words,
        "is_final": true,
        "speech_final": speechFinal,
        "itn_applied": state.itnEnabled,
        "from_finalize": fromFinalize
    ]
    if let data = try? JSONSerialization.data(withJSONObject: finalEvent),
       let json = String(data: data, encoding: .utf8) {
        try? await ws.send(json)
    }
}

// MARK: - Event Helpers

private func streamEventJSON(type: String, text: String, isFinal: Bool, start: Double, end: Double) -> String {
    let dict: [String: Any] = ["type": type, "text": text, "is_final": isFinal, "start": start, "end": end]
    if let data = try? JSONSerialization.data(withJSONObject: dict),
       let json = String(data: data, encoding: .utf8) {
        return json
    }
    return #"{"type":"error","message":"json encoding failed"}"#
}
