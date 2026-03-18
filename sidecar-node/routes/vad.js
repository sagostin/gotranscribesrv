/**
 * VAD route — POST /vad
 *
 * Accepts multipart audio upload and returns detected speech segments.
 * Uses parakeet-coreml's built-in Silero VAD (CoreML-accelerated).
 */

const { Router } = require("express")
const multer = require("multer")
const { execFile } = require("node:child_process")
const { writeFile, readFile, unlink } = require("node:fs/promises")
const { tmpdir } = require("node:os")
const { join } = require("node:path")
const { randomUUID } = require("node:crypto")

const upload = multer({ limits: { fileSize: 100 * 1024 * 1024 } })

/**
 * Convert audio bytes to 16kHz mono Float32 PCM using ffmpeg.
 */
async function audioToPcm(audioBuffer) {
  const inputPath = join(tmpdir(), `vad-in-${randomUUID()}`)
  const outputPath = join(tmpdir(), `vad-out-${randomUUID()}.pcm`)

  try {
    await writeFile(inputPath, audioBuffer)

    await new Promise((resolve, reject) => {
      execFile(
        "ffmpeg",
        ["-i", inputPath, "-ar", "16000", "-ac", "1", "-f", "f32le", "-y", outputPath],
        { timeout: 60_000 },
        (err, _stdout, stderr) => {
          if (err) reject(new Error(`ffmpeg failed: ${stderr || err.message}`))
          else resolve()
        }
      )
    })

    const pcmBuffer = await readFile(outputPath)
    return new Float32Array(
      pcmBuffer.buffer,
      pcmBuffer.byteOffset,
      pcmBuffer.length / 4
    )
  } finally {
    await unlink(inputPath).catch(() => {})
    await unlink(outputPath).catch(() => {})
  }
}

/**
 * @param {import('parakeet-coreml').ParakeetAsrEngine} engine
 */
function createVadRouter(engine) {
  const router = Router()

  router.post("/vad", upload.single("audio"), async (req, res, next) => {
    try {
      if (!req.file) {
        return res.status(422).json({
          error: {
            code: "MISSING_AUDIO",
            message: "Audio file is required (field: 'audio')",
          },
        })
      }

      const startMs = performance.now()

      const samples = await audioToPcm(req.file.buffer)

      // Use transcribe with VAD — segments contain speech regions
      const result = await engine.transcribe(samples)

      const processingTimeMs = Math.round(performance.now() - startMs)

      const segments = (result.segments || []).map((seg) => ({
        start: seg.startTime,
        end: seg.endTime,
      }))

      res.json({
        speech_segments: segments,
        duration: samples.length / 16000,
        processing_time_ms: processingTimeMs,
      })
    } catch (err) {
      next(err)
    }
  })

  return router
}

module.exports = { createVadRouter }
