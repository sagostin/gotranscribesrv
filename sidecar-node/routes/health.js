/**
 * Health route — GET /health
 *
 * Returns engine readiness and model info.
 */

const { Router } = require("express")

/**
 * @param {import('parakeet-coreml').ParakeetAsrEngine} engine
 */
function createHealthRouter(engine) {
  const router = Router()

  router.get("/health", (_req, res) => {
    const ready = engine.isReady()
    res.status(ready ? 200 : 503).json({
      status: ready ? "ok" : "initializing",
      models: {
        asr: ready ? "loaded" : "not loaded",
        vad: ready ? "loaded" : "not loaded",
      },
      runtime: "coreml",
      version: engine.getVersion(),
    })
  })

  return router
}

module.exports = { createHealthRouter }
