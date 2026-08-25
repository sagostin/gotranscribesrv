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

/// Segment length that triggers a rolling mid-utterance final (~10s).
/// Deepgram commits is_final=true (speech_final=false) results for long
/// segments even without a detected pause; clients that commit text on
/// is_final cadence starve without them.
private let maxSegmentSamples = 10 * 16_000

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
    /// Number of final events emitted this session — used to suppress the
    /// content-free closing final once real finals have gone out.
    private var _finalsEmitted = 0
    /// Sample index where the VAD last saw speech end, awaiting the
    /// endpointing window. nil when speech is active or no endpoint pending.
    private var _pendingSpeechEnd: Int?
    /// Bumped whenever the pending endpoint changes so stale endpoint
    /// watchdogs no-op.
    private var _endpointGeneration = 0

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

    /// Record a final event sent to the client.
    func markFinalSent() {
        lock.lock()
        _finalsEmitted += 1
        lock.unlock()
    }

    var finalsEmitted: Int {
        lock.lock()
        defer { lock.unlock() }
        return _finalsEmitted
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

/// Serializes ASR transcription calls within a session. The shared CoreML
/// engine does not tolerate concurrent transcribe() calls — they fail
/// intermittently ("transcription failed") — and a CloseStream that races
/// an in-flight endpoint final used to close the socket before the final
/// was sent, losing that segment for the client.
///
/// Partials use tryAcquire (skippable — the next interval catches up);
/// every final path uses acquire (must not be dropped).
private actor TranscriptionGate {
    private var busy = false

    /// Non-blocking attempt. Returns false when a transcription is running.
    func tryAcquire() -> Bool {
        if busy { return false }
        busy = true
        return true
    }

    /// Blocking acquire — waits for the in-flight transcription.
    func acquire() async {
        while busy {
            try? await Task.sleep(nanoseconds: 5_000_000)
        }
        busy = true
    }

    func release() {
        busy = false
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
    let gate = TranscriptionGate()

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
        await processVad(samples16k, ws: ws, state: state, engines: engines, gate: gate, logger: req.logger)

        // Rolling final: the segment has grown past maxSegmentSamples
        // without an endpoint — commit it (is_final, speech_final=false)
        // like Deepgram's mid-utterance finalization, then start a new
        // segment so partials/finals stay stream-relative.
        if state.segmentSampleCount >= maxSegmentSamples {
            await emitRollingFinal(ws: ws, state: state, engines: engines, gate: gate, logger: req.logger)
            return
        }

        // Check if we have enough new audio for a partial transcription
        let newCount = state.newSampleCount
        let segmentCount = state.segmentSampleCount
        if newCount >= partialIntervalSamples && segmentCount >= minTranscribeSamples {
            // Skip if a transcription is already in flight (e.g. an
            // endpoint final) — the next interval will catch up.
            guard await gate.tryAcquire() else { return }
            req.logger.info("[STREAM] Triggering partial transcription (\(state.sampleCount) samples, \(String(format: "%.1f", Double(state.sampleCount) / 16000.0))s)")
            let segment = state.segmentSamples
            let segmentStart = state.finalizedUpTo
            state.markTranscribed()

            // Same leading-silence trim as finals: Deepgram's interims and
            // the final that supersedes them share the segment start — a
            // client that matches a final to its live segment by `start`
            // breaks when the final jumps right of the interims.
            let result = await transcribeSegment(segment, state: state, engines: engines, logger: req.logger, label: "partial")
            await gate.release()
            guard let result else { return } // failure already logged
            guard !result.text.isEmpty else {
                req.logger.debug("[STREAM] Partial transcribed empty, skipping (\(segment.count) samples @ \(String(format: "%.2f", Double(segmentStart) / 16000.0))s)")
                return
            }
            let startSec = Double(segmentStart + result.trimmedSamples) / 16000.0
            let endSec = Double(segmentStart + segment.count) / 16000.0
            let words = buildWords(tokenTimings: result.tokenTimings, startOffsetSec: startSec, itnEnabled: state.itnEnabled, diarizeEnabled: state.diarizeEnabled)
            let partial = streamEventJSON(type: "partial", text: result.text, isFinal: false, start: startSec, end: endSec, words: words)
            try? await ws.send(partial)
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
                await finalizeStream(ws: ws, state: state, engines: engines, gate: gate, logger: req.logger)
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
                    await emitFinalEvent(ws: ws, state: state, engines: engines, gate: gate, logger: req.logger, fromFinalize: true, speechFinal: speechFinal)
                case "CloseStream":
                    req.logger.info("[STREAM] CloseStream, finalizing (\(state.sampleCount) samples)")
                    state.close()
                    await finalizeStream(ws: ws, state: state, engines: engines, gate: gate, logger: req.logger)
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
    gate: TranscriptionGate,
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
                    scheduleEndpointWatchdog(ws: ws, state: state, engines: engines, gate: gate, logger: logger, generation: generation)
                }
            }
        } catch {
            logger.warning("[STREAM] VAD chunk failed: \(error)")
        }

        // Endpoint check after each drained chunk (256ms granularity).
        if state.endpointDue() != nil {
            await emitEndpointFinal(ws: ws, state: state, engines: engines, gate: gate, logger: logger)
        }
    }
}

/// Fires a pending endpoint even when no further audio frames arrive. The
/// endpoint is due endpointing-ms after the speech end was DETECTED (wall
/// clock) — the VAD sample counter can stall when the client stops
/// sending, so the watchdog deliberately does not depend on it. No-ops if
/// the endpoint was already consumed/canceled.
private func scheduleEndpointWatchdog(
    ws: WebSocket,
    state: StreamState,
    engines: EngineManager,
    gate: TranscriptionGate,
    logger: Logger,
    generation: Int
) {
    guard let threshold = state.endpointingSamples else { return }
    let waitNs = UInt64(threshold / 16 + 300) * 1_000_000 // endpointing + VAD chunk slack
    Task {
        for _ in 0..<10 {
            try? await Task.sleep(nanoseconds: waitNs)
            guard !state.sessionClosed, state.endpointPending(generation: generation) else { return }
            await emitEndpointFinal(ws: ws, state: state, engines: engines, gate: gate, logger: logger)
            return
        }
    }
}

/// Emit the segment final for a detected endpoint: transcribe the current
/// segment, send final (speech_final=true) + utterance_end, start a new
/// segment. Waits on the transcription gate — endpoint finals must never
/// be dropped or raced against a CloseStream final.
private func emitEndpointFinal(
    ws: WebSocket,
    state: StreamState,
    engines: EngineManager,
    gate: TranscriptionGate,
    logger: Logger
) async {
    guard let speechEnd = state.pendingSpeechEnd else { return }
    await gate.acquire()
    state.clearPendingEndpoint()

    let segment = state.segmentSamples
    let segmentStart = state.finalizedUpTo
    let lastWordEnd = Double(speechEnd) / 16000.0

    // False VAD trigger on a tiny blip — just reset the segment.
    guard segment.count >= minFinalSamples else {
        state.markFinalized()
        await gate.release()
        return
    }

    logger.info("[STREAM] Endpoint detected — finalizing segment (\(segment.count) samples, \(String(format: "%.1f", Double(segment.count) / 16000.0))s)")

    let result = await transcribeSegment(segment, state: state, engines: engines, logger: logger, label: "endpoint")
    if let result, !result.text.isEmpty {
        await sendFinal(ws: ws, state: state, result: result, segmentStart: segmentStart, segmentSampleCount: segment.count, fromFinalize: false, speechFinal: true)
        state.markFinalSent()
    } else {
        // Visible forever: an endpoint that produces no text used to be
        // indistinguishable from endpointing not working at all.
        logger.warning("[STREAM] Endpoint segment transcribed EMPTY/failed (\(segment.count) samples @ \(String(format: "%.2f", Double(segmentStart) / 16000.0))s)")
    }
    state.markFinalized()
    await gate.release()
    logger.info("[STREAM] Segment finalized (endpoint) — new segment starts at \(String(format: "%.2f", Double(state.finalizedUpTo) / 16000.0))s (finals=\(state.finalsEmitted))")
    try? await ws.send(#"{"type":"utterance_end","last_word_end":"# + "\(lastWordEnd)}")
}

/// Emit a rolling mid-utterance final for an over-long segment: transcribe,
/// send final with speech_final=FALSE (speech is still going — Deepgram
/// semantics), and advance the segment boundary. No utterance_end: the
/// speaker hasn't paused. Waits on the transcription gate like any final.
private func emitRollingFinal(
    ws: WebSocket,
    state: StreamState,
    engines: EngineManager,
    gate: TranscriptionGate,
    logger: Logger
) async {
    await gate.acquire()
    let segment = state.segmentSamples
    let segmentStart = state.finalizedUpTo

    guard segment.count >= minFinalSamples else {
        state.markFinalized()
        await gate.release()
        return
    }

    logger.info("[STREAM] Segment exceeds \(maxSegmentSamples / 16000)s without endpoint — rolling final (\(segment.count) samples, \(String(format: "%.1f", Double(segment.count) / 16000.0))s)")

    let result = await transcribeSegment(segment, state: state, engines: engines, logger: logger, label: "rolling")
    if let result, !result.text.isEmpty {
        await sendFinal(ws: ws, state: state, result: result, segmentStart: segmentStart, segmentSampleCount: segment.count, fromFinalize: false, speechFinal: false)
        state.markFinalSent()
    } else {
        logger.warning("[STREAM] Rolling segment transcribed EMPTY/failed (\(segment.count) samples @ \(String(format: "%.2f", Double(segmentStart) / 16000.0))s)")
    }
    state.markFinalized()
    await gate.release()
    logger.info("[STREAM] Segment finalized (rolling) — new segment starts at \(String(format: "%.2f", Double(state.finalizedUpTo) / 16000.0))s (finals=\(state.finalsEmitted))")
}

/// Perform final transcription, send results, then signal done and close.
/// Waits for any in-flight endpoint/partial transcription first — the
/// endpoint final must reach the client BEFORE done is sent and the
/// socket closes (previously the racing close could drop it).
private func finalizeStream(
    ws: WebSocket, state: StreamState, engines: EngineManager, gate: TranscriptionGate, logger: Logger
) async {
    // Deepgram semantics: the close-forced final is speech_final only when
    // the VAD actually saw speech end — not unconditionally.
    let speechFinal = state.pendingSpeechEnd != nil
    await emitFinalEvent(ws: ws, state: state, engines: engines, gate: gate, logger: logger, fromFinalize: false, speechFinal: speechFinal)

    try? await ws.send(#"{"type":"done"}"#)
    try? await ws.close()
}

/// Transcribe the current (unfinalized) segment and emit a single `final`
/// event. Used by CloseStream (fromFinalize=false, then done+close) and
/// Finalize (fromFinalize=true, session stays open).
///
/// Waits on the transcription gate — a CloseStream that races an in-flight
/// endpoint final must queue behind it, not corrupt the shared engine or
/// close the socket before the endpoint final is sent.
///
/// Deepgram parity on the closing final: when endpointing already
/// finalized all speech, the remaining segment is empty — suppress the
/// content-free closing final in that case (Deepgram sends the terminal
/// Metadata without a redundant empty Results). Finalize always answers —
/// the client is explicitly waiting for a flush response.
private func emitFinalEvent(
    ws: WebSocket, state: StreamState, engines: EngineManager, gate: TranscriptionGate, logger: Logger,
    fromFinalize: Bool, speechFinal: Bool
) async {
    state.clearPendingEndpoint()
    await gate.acquire()
    defer { Task { await gate.release() } }

    // Snapshot AFTER acquiring the gate: an in-flight endpoint final may
    // have just advanced the segment boundary.
    let segment = state.segmentSamples
    let segmentStart = state.finalizedUpTo

    guard segment.count >= minFinalSamples else {
        if !fromFinalize && state.finalsEmitted > 0 {
            logger.info("[STREAM] CloseStream with \(segment.count) unfinalized samples after \(state.finalsEmitted) final(s) — suppressing empty closing final")
            state.markFinalized()
            return
        }
        logger.info("[STREAM] Not enough audio (\(segment.count) samples), sending empty final")
        await sendFinal(ws: ws, state: state, result: nil, segmentStart: segmentStart, segmentSampleCount: segment.count, fromFinalize: fromFinalize, speechFinal: speechFinal)
        state.markFinalSent()
        state.markFinalized()
        return
    }

    let result = await transcribeSegment(segment, state: state, engines: engines, logger: logger, label: "final")
    guard let result else {
        // transcribeSegment already logged; notify the client
        try? await ws.send(#"{"type":"error","message":"transcription failed"}"#)
        return
    }
    if result.text.isEmpty && !fromFinalize && state.finalsEmitted > 0 {
        // Same suppression for a segment that transcribes to nothing.
        logger.info("[STREAM] Closing segment transcribed empty after \(state.finalsEmitted) final(s) — suppressing")
        state.markFinalized()
        return
    }
    await sendFinal(ws: ws, state: state, result: result, segmentStart: segmentStart, segmentSampleCount: segment.count, fromFinalize: fromFinalize, speechFinal: speechFinal)
    state.markFinalSent()
    state.markFinalized()
}

/// Transcribe a segment with ITN applied. Leading digital silence is
/// trimmed first: segments begin where the previous final cut (mid-pause,
/// up to ~1s of dead air after VAD hysteresis + endpointing), and Parakeet
/// TDT can return empty text for short speech buried behind leading
/// silence — the production "closing final came back empty" failure mode.
/// The trim count is returned so word timings/stream offsets can be
/// corrected. Returns nil on failure.
private func transcribeSegment(
    _ segment: [Float],
    state: StreamState,
    engines: EngineManager,
    logger: Logger,
    label: String
) async -> (text: String, tokenTimings: [TokenTiming]?, trimmedSamples: Int)? {
    let (trimmed, trimCount) = trimLeadingSilence(segment)
    if trimCount > 0 {
        logger.info("[STREAM] \(label): trimmed \(trimCount) leading-silence samples (\(String(format: "%.2f", Double(trimCount) / 16000.0))s)")
    }
    guard !trimmed.isEmpty else {
        logger.info("[STREAM] \(label): segment is all silence — skipping ASR")
        return ("", nil, trimCount)
    }
    do {
        let result = try await engines.transcribe(trimmed)
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
        return (finalText, result.tokenTimings, trimCount)
    } catch {
        logger.error("[STREAM] \(label.capitalized) transcription failed: \(error)")
        return nil
    }
}

/// Drop leading silence from a segment before transcription, keeping a
/// 200ms pre-roll for acoustic context. 20ms windows; "loud" is RMS above
/// ~-46dBFS (telephony quiet speech is ~-15dBFS, mulaw digital silence is
/// true zero).
private func trimLeadingSilence(_ samples: [Float], threshold: Float = 0.005, preRoll: Int = 3200) -> ([Float], Int) {
    let window = 320 // 20ms @ 16kHz
    var firstLoud = 0
    var i = 0
    let thresh2 = threshold * threshold
    while i + window <= samples.count {
        var energy: Float = 0
        for j in i..<(i + window) { energy += samples[j] * samples[j] }
        if energy / Float(window) > thresh2 {
            firstLoud = i
            break
        }
        i += window
    }
    if i + window > samples.count {
        // Never found a loud window — all silence.
        return ([], samples.count)
    }
    let cut = max(0, firstLoud - preRoll)
    return (Array(samples[cut...]), cut)
}

/// Build and send a `final` event for a transcribed segment. Word timings
/// are built from token timings and offset to stream-relative seconds
/// (Deepgram semantics). `trimSeconds` is the leading silence cut before
/// transcription — the result's start (and every word) shifts right by it.
/// A nil result produces the empty final used to answer Finalize on
/// too-short audio.
private func sendFinal(
    ws: WebSocket,
    state: StreamState,
    result: (text: String, tokenTimings: [TokenTiming]?, trimmedSamples: Int)?,
    segmentStart: Int,
    segmentSampleCount: Int,
    fromFinalize: Bool,
    speechFinal: Bool
) async {
    let trimSeconds = Double(result?.trimmedSamples ?? 0) / 16000.0
    let startSec = Double(segmentStart) / 16000.0 + trimSeconds
    let endSec = Double(segmentStart + segmentSampleCount) / 16000.0

    let words = buildWords(tokenTimings: result?.tokenTimings, startOffsetSec: startSec, itnEnabled: state.itnEnabled, diarizeEnabled: state.diarizeEnabled)

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

/// Build the words array from token timings (per-word ITN applies to the
/// surface text). Token times are relative to the transcribed (trimmed)
/// audio — startOffsetSec shifts them to stream-relative seconds (Deepgram
/// semantics). Shared by partial and final events so interims carry the
/// same words shape as finals.
private func buildWords(tokenTimings: [TokenTiming]?, startOffsetSec: Double, itnEnabled: Bool, diarizeEnabled: Bool) -> [[String: Any]] {
    var words: [[String: Any]] = []
    guard let timings = tokenTimings else { return words }
    let itn = TextNormalizer.shared
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
            var w: [String: Any] = ["word": surface, "start": wordStart + startOffsetSec, "end": wordEnd + startOffsetSec]
            if diarizeEnabled { w["speaker"] = "0" }
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
        var w: [String: Any] = ["word": surface, "start": wordStart + startOffsetSec, "end": wordEnd + startOffsetSec]
        if diarizeEnabled { w["speaker"] = "0" }
        words.append(w)
    }
    return words
}

private func streamEventJSON(type: String, text: String, isFinal: Bool, start: Double, end: Double, words: [[String: Any]] = []) -> String {
    let dict: [String: Any] = ["type": type, "text": text, "is_final": isFinal, "start": start, "end": end, "words": words]
    if let data = try? JSONSerialization.data(withJSONObject: dict),
       let json = String(data: data, encoding: .utf8) {
        return json
    }
    return #"{"type":"error","message":"json encoding failed"}"#
}
