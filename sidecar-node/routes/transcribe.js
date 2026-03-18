/**
 * Transcribe route — POST /transcribe
 *
 * Accepts multipart audio upload (field: "audio"), converts to
 * 16kHz mono Float32 PCM via ffmpeg, runs through parakeet-coreml,
 * and returns a JSON transcript matching the Python sidecar format.
 */

const { Router } = require("express")
const multer = require("multer")
const { execFile } = require("node:child_process")
const { writeFile, readFile, unlink } = require("node:fs/promises")
const { tmpdir } = require("node:os")
const { join } = require("node:path")
const { randomUUID } = require("node:crypto")

const upload = multer({ limits: { fileSize: 100 * 1024 * 1024 } }) // 100MB

/**
 * Convert audio bytes to 16kHz mono Float32 PCM using ffmpeg.
 * @param {Buffer} audioBuffer - Raw audio file bytes (any format)
 * @returns {Promise<Float32Array>} - PCM samples
 */
async function audioToPcm(audioBuffer) {
  const inputPath = join(tmpdir(), `asr-in-${randomUUID()}`)
  const outputPath = join(tmpdir(), `asr-out-${randomUUID()}.pcm`)

  try {
    await writeFile(inputPath, audioBuffer)

    await new Promise((resolve, reject) => {
      execFile(
        "ffmpeg",
        [
          "-i", inputPath,
          "-ar", "16000",
          "-ac", "1",
          "-f", "f32le",
          "-y",
          outputPath,
        ],
        { timeout: 60_000 },
        (err, _stdout, stderr) => {
          if (err) {
            reject(new Error(`ffmpeg failed: ${stderr || err.message}`))
          } else {
            resolve()
          }
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
    // Cleanup temp files
    await unlink(inputPath).catch(() => {})
    await unlink(outputPath).catch(() => {})
  }
}

/**
 * @param {import('parakeet-coreml').ParakeetAsrEngine} engine
 */
function createTranscribeRouter(engine) {
  const router = Router()

  router.post("/transcribe", upload.single("audio"), async (req, res, next) => {
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

      // Convert to 16kHz PCM
      const samples = await audioToPcm(req.file.buffer)

      // Transcribe via CoreML/ANE
      const result = await engine.transcribe(samples)

      const processingTimeMs = Math.round(performance.now() - startMs)

      // Format response to match Python sidecar schema
      // parakeet-coreml TranscriptionResult:
      //   { text, durationMs, segments: [{ startTime, endTime, text }] }
      const segments = (result.segments || []).map((seg) => ({
        start: seg.startTime,
        end: seg.endTime,
        text: seg.text,
      }))

      const lastSegment = segments[segments.length - 1]
      const audioDuration = lastSegment ? lastSegment.end : 0

      const response = {
        text: result.text,
        segments,
        words: [], // parakeet-coreml doesn't provide word-level timestamps
        duration: audioDuration,
        processing_time_ms: processingTimeMs,
        model: "parakeet-tdt-coreml",
        diarized: false,
      }

      res.json(response)
    } catch (err) {
      next(err)
    }
  })

  return router
}

module.exports = { createTranscribeRouter }
