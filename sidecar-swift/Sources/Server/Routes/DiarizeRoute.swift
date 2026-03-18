import FluidAudio
import Vapor

/// Standalone diarization route — POST /diarize
func diarizeRoutes(_ app: Application, engines: EngineManager) {
    app.on(.POST, "diarize", body: .collect(maxSize: "100mb")) { req async throws -> TranscribeResponse in
        try await handleDiarize(req: req, engines: engines)
    }
}

private func handleDiarize(req: Request, engines: EngineManager) async throws -> TranscribeResponse {
    let upload = try req.content.decode(DiarizeUpload.self)
    let audioData = Data(buffer: upload.audio.data)

    guard !audioData.isEmpty else {
        throw Abort(.unprocessableEntity, reason: "Audio file is required (field: 'audio')")
    }

    guard let transcriptData = upload.transcript.data(using: .utf8) else {
        throw Abort(.badRequest, reason: "Invalid transcript encoding")
    }

    let transcript = try JSONDecoder().decode(TranscribeResponse.self, from: transcriptData)

    let startTime = ContinuousClock.now

    let samples = try await SidecarAudioConverter.toPCM16kMono(audioData)

    // Run diarization inside actor (Sortformer)
    let diarSegs = try await engines.diarize(samples)

    let elapsed = ContinuousClock.now - startTime
    let diarizationMs = Int(elapsed.components.seconds * 1000)
        + Int(elapsed.components.attoseconds / 1_000_000_000_000_000)

    // Enrich transcript with speaker labels
    var enrichedSegments: [TranscribeSegment] = []
    var enrichedWords: [TranscribeWord] = []
    var speakerStats: [String: SpeakerSummary] = [:]

    for segment in transcript.segments {
        let speaker = dominantSpeaker(
            start: segment.start, end: segment.end, diarSegments: diarSegs
        )
        enrichedSegments.append(TranscribeSegment(
            speaker: speaker, start: segment.start, end: segment.end, text: segment.text
        ))
        let duration = segment.end - segment.start
        let wordCount = segment.text.split(separator: " ").count
        if var existing = speakerStats[speaker] {
            existing.segment_count += 1
            existing.word_count += wordCount
            existing.total_duration_s += duration
            speakerStats[speaker] = existing
        } else {
            speakerStats[speaker] = SpeakerSummary(
                segment_count: 1, word_count: wordCount, total_duration_s: duration
            )
        }
    }

    for word in transcript.words {
        let speaker = dominantSpeaker(
            start: word.start, end: word.end, diarSegments: diarSegs
        )
        enrichedWords.append(TranscribeWord(
            word: word.word, start: word.start, end: word.end, speaker: speaker
        ))
    }

    // Merge consecutive same-speaker segments
    let mergedSegments = mergeSameSpeakerSegments(enrichedSegments)

    return TranscribeResponse(
        text: transcript.text,
        segments: mergedSegments,
        words: enrichedWords,
        duration: transcript.duration,
        processing_time_ms: transcript.processing_time_ms + diarizationMs,
        model: transcript.model,
        diarized: true,
        num_speakers: speakerStats.count,
        speakers: speakerStats
    )
}

private func mergeSameSpeakerSegments(_ segments: [TranscribeSegment]) -> [TranscribeSegment] {
    guard segments.count > 1 else { return segments }

    var merged: [TranscribeSegment] = [segments[0]]

    for seg in segments.dropFirst() {
        let prev = merged[merged.count - 1]
        if seg.speaker == prev.speaker && (seg.start - prev.end) < 2.0 {
            merged[merged.count - 1] = TranscribeSegment(
                speaker: prev.speaker,
                start: prev.start,
                end: seg.end,
                text: (prev.text + " " + seg.text).trimmingCharacters(in: .whitespaces)
            )
        } else {
            merged.append(seg)
        }
    }

    return merged
}

// MARK: - Models

struct DiarizeUpload: Content {
    var audio: File
    var transcript: String
}
