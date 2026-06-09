import FluidAudio
import Vapor

/// GoTranscribeSrv — Swift Inference Sidecar
///
/// HTTP server providing ASR, VAD, diarization, and TTS endpoints
/// powered by FluidAudio (CoreML/Apple Neural Engine).
///
/// Replaces both the Python sidecar (diarization, TTS) and the
/// Node.js sidecar (ASR, VAD) with a single, high-performance
/// Swift service.

let env = try Environment.detect()
let app = try await Application.make(env)

// Configure port (default: 8101, matching the old ASR sidecar)
let port = Int(Environment.get("AUDIO_SIDECAR_PORT") ?? "8101") ?? 8101
let host = Environment.get("AUDIO_SIDECAR_HOST") ?? "0.0.0.0"
app.http.server.configuration.port = port
app.http.server.configuration.hostname = host

// Allow large uploads (100MB for long audio files)
app.routes.defaultMaxBodySize = "100mb"

print("🚀 Initializing FluidAudio engines (CoreML/ANE)...")

// Initialize all engines
let engines = EngineManager()
await engines.initialize()

let status = await engines.healthStatus()
let loaded = status.filter { $0.value == "loaded" }.map { $0.key }
print("✅ Startup complete — loaded engines: \(loaded)")

// ITN status (FluidAudio TextNormalizer)
// - With libnemo_text_processing linked: full NeMo ITN (spoken → written form)
// - Without: Swift passthrough (normalize() returns input unchanged)
// Either way, ITN is safe to enable by default.
let itn = TextNormalizer.shared
if itn.isNativeAvailable {
    print("📝 ITN: NeMo library loaded (version=\(itn.version ?? "unknown"))")
} else {
    print("📝 ITN: Swift fallback (libnemo_text_processing not linked — passthrough)")
}

// Register routes
healthRoutes(app, engines: engines)
transcribeRoutes(app, engines: engines)
streamRoutes(app, engines: engines)
vadRoutes(app, engines: engines)
diarizeRoutes(app, engines: engines)
ttsRoutes(app, engines: engines)

print("🎙  Swift sidecar listening on \(host):\(port)")
print("   POST /transcribe  — ASR (+ optional diarization)")
print("   WS   /stream      — real-time streaming ASR")
print("   POST /vad         — voice activity detection")
print("   POST /diarize     — speaker diarization")
print("   POST /synthesize  — text-to-speech")
print("   GET  /voices      — list TTS voice presets")
print("   GET  /health      — engine status")

try await app.execute()
