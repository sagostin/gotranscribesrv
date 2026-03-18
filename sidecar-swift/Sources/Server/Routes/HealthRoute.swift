import Vapor

/// Health check route — reports engine status.
func healthRoutes(_ app: Application, engines: EngineManager) {
    app.get("health") { req async -> HealthResponse in
        let status = await engines.healthStatus()
        return HealthResponse(status: "ok", models: status)
    }
}

struct HealthResponse: Content {
    let status: String
    let models: [String: String]
}
