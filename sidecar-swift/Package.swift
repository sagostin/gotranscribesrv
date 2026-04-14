// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "SidecarSwift",
    platforms: [
        .macOS(.v14),
    ],
    dependencies: [
        // FluidAudio — CoreML-optimized ASR, VAD, Diarization, TTS
        .package(url: "https://github.com/FluidInference/FluidAudio.git", exact: "0.12.6"),
        // Vapor — HTTP server with WebSocket, multipart, JSON support
        .package(url: "https://github.com/vapor/vapor.git", from: "4.99.0"),
    ],
    targets: [
        .executableTarget(
            name: "Server",
            dependencies: [
                .product(name: "FluidAudio", package: "FluidAudio"),
                .product(name: "Vapor", package: "vapor"),
            ],
            path: "Sources/Server"
        ),
    ]
)
