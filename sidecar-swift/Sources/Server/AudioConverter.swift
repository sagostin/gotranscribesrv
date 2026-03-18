import FluidAudio
import Vapor

/// Audio conversion utility — wraps FluidAudio's AudioConverter
/// to handle Data → [Float] conversion via a temp file.
struct SidecarAudioConverter {
    /// Convert arbitrary audio bytes to 16kHz mono Float32 samples.
    static func toPCM16kMono(_ data: Data) async throws -> [Float] {
        let tempURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("sidecar-in-\(UUID().uuidString).audio")

        defer {
            try? FileManager.default.removeItem(at: tempURL)
        }

        try data.write(to: tempURL)

        // FluidAudio's AudioConverter handles format detection + resampling
        let converter = AudioConverter()
        let samples = try converter.resampleAudioFile(tempURL)

        guard !samples.isEmpty else {
            throw Abort(.unprocessableEntity, reason: "Audio conversion produced no samples")
        }

        return samples
    }
}
