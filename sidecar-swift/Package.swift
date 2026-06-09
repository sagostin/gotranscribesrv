// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "SidecarSwift",
    platforms: [
        .macOS(.v14),
    ],
    dependencies: [
        // FluidAudio — CoreML-optimized ASR, VAD, Diarization, TTS
        .package(url: "https://github.com/FluidInference/FluidAudio.git", exact: "0.13.6"),
        // Vapor — HTTP server with WebSocket, multipart, JSON support
        .package(url: "https://github.com/vapor/vapor.git", from: "4.99.0"),
    ],
    targets: [
        // ITN library — pure-Swift inverse text normalization.
        // Exposed as a library target so it can be unit-tested in isolation
        // and (optionally) consumed by other targets.
        .target(
            name: "ITN",
            path: "Sources/ITN"
        ),
        .executableTarget(
            name: "Server",
            dependencies: [
                .product(name: "FluidAudio", package: "FluidAudio"),
                .product(name: "Vapor", package: "vapor"),
                "ITN",
            ],
            path: "Sources/Server"
        ),
        .testTarget(
            name: "ITNTests",
            dependencies: ["ITN"],
            path: "Tests/ITNTests"
        ),
    ]
)
