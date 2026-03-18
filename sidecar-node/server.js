/**
 * GoTranscribeSrv — Node.js ASR Sidecar
 *
 * Express server providing ASR (Parakeet TDT via CoreML/ANE) and VAD
 * (Silero via CoreML) endpoints. Communicates with the Go backend
 * over localhost HTTP.
 *
 * Runs on Apple's Neural Engine — no GPU required.
 */

const express = require("express")
const { ParakeetAsrEngine } = require("parakeet-coreml")
const { createTranscribeRouter } = require("./routes/transcribe")
const { createVadRouter } = require("./routes/vad")
const { createHealthRouter } = require("./routes/health")

const PORT = parseInt(process.env.ASR_SIDECAR_PORT || "8101", 10)
const HOST = process.env.ASR_SIDECAR_HOST || "::" // dual-stack: IPv4 + IPv6

async function main() {
  console.log("Initializing ASR engine (CoreML/ANE)...")

  const engine = new ParakeetAsrEngine()
  await engine.initialize()

  console.log(`ASR engine ready (version: ${engine.getVersion()})`)

  const app = express()

  // Routes
  app.use(createHealthRouter(engine))
  app.use(createTranscribeRouter(engine))
  app.use(createVadRouter(engine))

  // Error handler
  app.use((err, _req, res, _next) => {
    console.error("Unhandled error:", err)
    res.status(500).json({
      error: {
        code: "INTERNAL_ERROR",
        message: err.message || "Internal server error",
      },
    })
  })

  app.listen(PORT, HOST, () => {
    console.log(`ASR sidecar listening on ${HOST}:${PORT}`)
    console.log(`  POST /transcribe  — file-based ASR`)
    console.log(`  POST /vad         — voice activity detection`)
    console.log(`  GET  /health      — engine status`)
  })

  // Graceful shutdown
  for (const signal of ["SIGINT", "SIGTERM"]) {
    process.on(signal, () => {
      console.log(`Received ${signal}, shutting down...`)
      engine.cleanup()
      process.exit(0)
    })
  }
}

main().catch((err) => {
  console.error("Fatal: failed to start ASR sidecar:", err)
  process.exit(1)
})
