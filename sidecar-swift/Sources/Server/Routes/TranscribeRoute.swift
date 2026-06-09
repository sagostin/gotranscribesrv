import FluidAudio
import Vapor

/// ASR transcription route — POST /transcribe
func transcribeRoutes(_ app: Application, engines: EngineManager) {
    app.on(.POST, "transcribe", body: .collect(maxSize: "100mb")) { req async throws -> TranscribeResponse in
        try await handleTranscribe(req: req, engines: engines)
    }
}

private func handleTranscribe(req: Request, engines: EngineManager) async throws -> TranscribeResponse {
    req.logger.info("POST /transcribe — decoding multipart upload")

    let audio = try req.content.decode(AudioUpload.self)
    let audioData = Data(buffer: audio.audio.data)

    req.logger.info("Received audio: \(audioData.count) bytes, filename: \(audio.audio.filename)")

    guard !audioData.isEmpty else {
        throw Abort(.unprocessableEntity, reason: "Audio file is required (field: 'audio')")
    }

    let startTime = ContinuousClock.now

    // Convert to 16kHz mono PCM
    req.logger.info("Converting audio to 16kHz PCM...")
    let samples = try await SidecarAudioConverter.toPCM16kMono(audioData)
    req.logger.info("Audio converted: \(samples.count) samples (\(String(format: "%.1f", Double(samples.count) / 16000))s)")

    // Transcribe via CoreML/ANE (runs inside actor)
    req.logger.info("Running ASR inference (CoreML/ANE)...")
    let result = try await engines.transcribe(samples)
    req.logger.info("ASR complete: \(result.text.prefix(100))...")

    let elapsed = ContinuousClock.now - startTime
    let processingTimeMs = Int(elapsed.components.seconds * 1000)
        + Int(elapsed.components.attoseconds / 1_000_000_000_000_000)

    // Build word-level data from tokenTimings.
    // FluidAudio normalizes ▁ → space before returning tokenTimings.
    // Word boundaries are marked by leading " " or "▁" (FluidAudio's isWordBoundary).
    // Tokens WITHOUT a prefix are sub-word continuations.
    var words: [TranscribeWord] = []
    if let timings = result.tokenTimings {
        var currentWord = ""
        var wordStart: Double = 0
        var wordEnd: Double = 0

        for t in timings {
            let token = t.token

            // Skip special tokens
            if token.isEmpty || token == "<blank>" || token == "<pad>" { continue }

            // FluidAudio uses both ▁ and space as word boundary markers
            let isWordStart = token.hasPrefix("▁") || token.hasPrefix(" ")

            // Strip the boundary prefix to get the actual text
            let cleaned: String
            if isWordStart {
                cleaned = String(token.dropFirst()).trimmingCharacters(in: .whitespaces)
            } else {
                cleaned = token.trimmingCharacters(in: .whitespaces)
            }

            guard !cleaned.isEmpty else { continue }

            if isWordStart && !currentWord.isEmpty {
                // Flush previous word
                words.append(TranscribeWord(
                    word: currentWord, start: wordStart, end: wordEnd, speaker: nil
                ))
                currentWord = cleaned
                wordStart = t.startTime
                wordEnd = t.endTime
            } else if isWordStart || currentWord.isEmpty {
                // First word or new word start
                currentWord = cleaned
                wordStart = t.startTime
                wordEnd = t.endTime
            } else {
                // Continuation of current word
                currentWord += cleaned
                wordEnd = t.endTime
            }
        }

        // Flush final word
        if !currentWord.isEmpty {
            words.append(TranscribeWord(
                word: currentWord, start: wordStart, end: wordEnd, speaker: nil
            ))
        }
    }

    // Build sentence-level segments by grouping tokens with gaps < 0.8s
    let segments = buildSegmentsFromWords(words)

    // ITN: per-request opt-out via form field `itn=false`. Default is on
    // (matches the server-wide ENABLE_ITN default in the Go server).
    // TextNormalizer is a no-op when the native libnemo_text_processing
    // dylib is not linked (returns input unchanged), so this is safe.
    let itnEnabled = (audio.itn ?? "true").lowercased() != "false"
    let itn = TextNormalizer.shared
    let normalizedText = itnEnabled ? itn.normalizeSentence(result.text) : result.text
    let normalizedSegments = itnEnabled ? segments.map { seg in
        TranscribeSegment(
            speaker: seg.speaker,
            start: seg.start,
            end: seg.end,
            text: itn.normalizeSentence(seg.text)
        )
    } : segments
    let normalizedWords = itnEnabled ? words.map { w in
        TranscribeWord(
            word: itn.normalize(w.word),
            start: w.start,
            end: w.end,
            speaker: w.speaker
        )
    } : words

    // FluidAudio's result.duration may be 0 for some models — fall back to sample count
    let audioDuration: Double
    if result.duration > 0 {
        audioDuration = result.duration
    } else {
        audioDuration = Double(samples.count) / 16000.0
    }

    let diarize = (audio.diarize ?? "").lowercased() == "true"

    var response = TranscribeResponse(
        text: normalizedText,
        segments: normalizedSegments,
        words: normalizedWords,
        duration: audioDuration,
        processing_time_ms: processingTimeMs,
        model: "parakeet-tdt-v3-coreml",
        diarized: false,
        num_speakers: nil,
        speakers: nil,
        itn_applied: itnEnabled
    )

    // Run diarization if requested (Sortformer — end-to-end neural, 4 speakers)
    let diarizerAvailable = await engines.hasDiarizer()
    if diarize && diarizerAvailable {
        do {
            req.logger.info("Running Sortformer diarization on \(samples.count) samples...")
            let diarSegs = try await engines.diarize(samples)

            // Debug: log raw diarization output
            let uniqueSpeakers = Set(diarSegs.map { $0.speakerIndex })
            req.logger.info("Diarization returned \(diarSegs.count) segments, \(uniqueSpeakers.count) unique speakers: \(uniqueSpeakers.sorted())")
            for (i, seg) in diarSegs.prefix(10).enumerated() {
                req.logger.info("  diar[\(i)]: speaker=\(seg.speakerIndex) \(String(format: "%.2f", seg.startTime))s-\(String(format: "%.2f", seg.endTime))s")
            }
            if diarSegs.count > 10 {
                req.logger.info("  ... and \(diarSegs.count - 10) more segments")
            }

            response = enrichTranscript(
                response: response,
                diarSegments: diarSegs,
                processingTimeMs: processingTimeMs
            )
        } catch {
            req.logger.warning("Diarization failed: \(error)")
        }
    }

    return response
}

/// Group words into segments by detecting pauses > threshold.
private func buildSegmentsFromWords(_ words: [TranscribeWord]) -> [TranscribeSegment] {
    guard !words.isEmpty else { return [] }

    let pauseThreshold: Double = 0.8
    var segments: [TranscribeSegment] = []
    var currentWords: [TranscribeWord] = [words[0]]

    for word in words.dropFirst() {
        let gap = word.start - (currentWords.last?.end ?? 0)
        if gap > pauseThreshold {
            // Flush current segment
            let text = currentWords.map(\.word).joined(separator: " ")
            segments.append(TranscribeSegment(
                speaker: nil,
                start: currentWords.first!.start,
                end: currentWords.last!.end,
                text: text
            ))
            currentWords = [word]
        } else {
            currentWords.append(word)
        }
    }

    // Flush final segment
    if !currentWords.isEmpty {
        let text = currentWords.map(\.word).joined(separator: " ")
        segments.append(TranscribeSegment(
            speaker: nil,
            start: currentWords.first!.start,
            end: currentWords.last!.end,
            text: text
        ))
    }

    return segments
}

/// A flat diarization segment from Sortformer.
typealias DiarSegment = (speakerIndex: Int, startTime: Float, endTime: Float)

/// Enrich transcript with diarization speaker segments.
/// Rebuilds segments from speaker-labeled words so that speaker changes
/// always produce segment boundaries.
private func enrichTranscript(
    response: TranscribeResponse,
    diarSegments: [DiarSegment],
    processingTimeMs: Int
) -> TranscribeResponse {
    // Step 1: Label every word with the dominant speaker
    var enrichedWords: [TranscribeWord] = []
    for word in response.words {
        let speaker = dominantSpeaker(
            start: word.start, end: word.end, diarSegments: diarSegments
        )
        enrichedWords.append(TranscribeWord(
            word: word.word, start: word.start, end: word.end, speaker: speaker
        ))
    }

    // Step 2: Build segments by grouping consecutive same-speaker words.
    // A new segment starts when the speaker changes OR there's a pause > 2s.
    var enrichedSegments: [TranscribeSegment] = []
    var speakerSummary: [String: SpeakerSummary] = [:]

    guard !enrichedWords.isEmpty else {
        return TranscribeResponse(
            text: response.text, segments: [], words: enrichedWords,
            duration: response.duration, processing_time_ms: processingTimeMs,
            model: response.model, diarized: true, num_speakers: 0, speakers: [:],
            itn_applied: response.itn_applied
        )
    }

    var currentSpeaker = enrichedWords[0].speaker ?? "SPEAKER_00"
    var currentWords: [TranscribeWord] = [enrichedWords[0]]

    for word in enrichedWords.dropFirst() {
        let wordSpeaker = word.speaker ?? "SPEAKER_00"
        let gap = word.start - (currentWords.last?.end ?? 0)

        // Split on speaker change or large pause
        if wordSpeaker != currentSpeaker || gap > 2.0 {
            // Flush current segment
            let text = currentWords.map(\.word).joined(separator: " ")
            let segment = TranscribeSegment(
                speaker: currentSpeaker,
                start: currentWords.first!.start,
                end: currentWords.last!.end,
                text: text
            )
            enrichedSegments.append(segment)
            updateSpeakerSummary(&speakerSummary, speaker: currentSpeaker, segment: segment)

            currentSpeaker = wordSpeaker
            currentWords = [word]
        } else {
            currentWords.append(word)
        }
    }

    // Flush final segment
    if !currentWords.isEmpty {
        let text = currentWords.map(\.word).joined(separator: " ")
        let segment = TranscribeSegment(
            speaker: currentSpeaker,
            start: currentWords.first!.start,
            end: currentWords.last!.end,
            text: text
        )
        enrichedSegments.append(segment)
        updateSpeakerSummary(&speakerSummary, speaker: currentSpeaker, segment: segment)
    }

    return TranscribeResponse(
        text: response.text,
        segments: enrichedSegments,
        words: enrichedWords,
        duration: response.duration,
        processing_time_ms: processingTimeMs,
        model: response.model,
        diarized: true,
        num_speakers: speakerSummary.count,
        speakers: speakerSummary,
        itn_applied: response.itn_applied
    )
}

private func updateSpeakerSummary(
    _ summary: inout [String: SpeakerSummary], speaker: String, segment: TranscribeSegment
) {
    let duration = segment.end - segment.start
    let wordCount = segment.text.split(separator: " ").count
    if var existing = summary[speaker] {
        existing.segment_count += 1
        existing.word_count += wordCount
        existing.total_duration_s += duration
        summary[speaker] = existing
    } else {
        summary[speaker] = SpeakerSummary(
            segment_count: 1, word_count: wordCount, total_duration_s: duration
        )
    }
}

/// Find the speaker with the most overlap in [start, end].
func dominantSpeaker(
    start: Double, end: Double, diarSegments: [DiarSegment]
) -> String {
    var bestSpeaker = "SPEAKER_UNKNOWN"
    var bestOverlap: Double = 0

    for seg in diarSegments {
        let overlapStart = max(start, Double(seg.startTime))
        let overlapEnd = min(end, Double(seg.endTime))
        let overlap = overlapEnd - overlapStart
        if overlap > bestOverlap {
            bestOverlap = overlap
            bestSpeaker = String(format: "SPEAKER_%02d", seg.speakerIndex)
        }
    }

    // Fallback: if no overlap found, find nearest segment
    if bestOverlap <= 0 && !diarSegments.isEmpty {
        let mid = (start + end) / 2
        var bestDist = Double.infinity
        for seg in diarSegments {
            let segMid = (Double(seg.startTime) + Double(seg.endTime)) / 2
            if abs(mid - segMid) < bestDist {
                bestDist = abs(mid - segMid)
                bestSpeaker = String(format: "SPEAKER_%02d", seg.speakerIndex)
            }
        }
    }

    return bestSpeaker
}

// MARK: - Request/Response Models

struct AudioUpload: Content {
    var audio: File
    var language: String?
    var diarize: String?  // Multipart form fields are always strings
    var itn: String?      // "false" disables inverse text normalization for this request
}

struct TranscribeResponse: Content {
    let text: String
    var segments: [TranscribeSegment]
    var words: [TranscribeWord]
    let duration: Double
    var processing_time_ms: Int
    let model: String
    var diarized: Bool
    var num_speakers: Int?
    var speakers: [String: SpeakerSummary]?
    var itn_applied: Bool
}

struct TranscribeSegment: Content {
    var speaker: String?
    let start: Double
    let end: Double
    let text: String
}

struct TranscribeWord: Content {
    let word: String
    let start: Double
    let end: Double
    var speaker: String?
}

struct SpeakerSummary: Content {
    var segment_count: Int
    var word_count: Int
    var total_duration_s: Double
}
