// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "SidecarLLM",
    platforms: [
        .macOS(.v15)
    ],
    dependencies: [
        .package(url: "https://github.com/vapor/vapor.git", from: "4.99.0"),
        // 1.3.x includes stateful CoreML model support (merged from the preview branch).
        .package(url: "https://github.com/huggingface/swift-transformers", from: "1.3.3"),
        .package(url: "https://github.com/apple/ml-stable-diffusion.git", from: "1.1.0"),
        // Vendored copy of jkrukowski/swift-embeddings 0.1.0 with platforms bumped
        // to macOS 15 (upstream declares 14, which conflicts with swift-transformers
        // preview's macOS 15 requirement).
        .package(path: "vendor/swift-embeddings"),
        .package(url: "https://github.com/john-rocky/CoreML-LLM.git", branch: "main"),
    ],
    targets: [
        .target(
            name: "ModelRuntime",
            dependencies: [
                .product(name: "Transformers", package: "swift-transformers"),
            ],
            path: "Sources/ModelRuntime"
        ),
        .target(
            name: "Tooling",
            dependencies: ["ModelRuntime"],
            path: "Sources/Tooling"
        ),
        .target(
            name: "ImageRuntime",
            dependencies: [
                "ModelRuntime",
                .product(name: "StableDiffusion", package: "ml-stable-diffusion"),
            ],
            path: "Sources/ImageRuntime"
        ),
        .target(
            name: "EmbeddingRuntime",
            dependencies: [
                "ModelRuntime",
                .product(name: "Embeddings", package: "swift-embeddings"),
            ],
            path: "Sources/EmbeddingRuntime"
        ),
        .target(
            name: "ExternalRuntime",
            dependencies: [
                "ModelRuntime",
                .product(name: "CoreMLLLM", package: "CoreML-LLM"),
            ],
            path: "Sources/ExternalRuntime"
        ),
        .executableTarget(
            name: "Server",
            dependencies: [
                "ModelRuntime",
                "Tooling",
                "ImageRuntime",
                "EmbeddingRuntime",
                "ExternalRuntime",
                .product(name: "Vapor", package: "vapor"),
            ],
            path: "Sources/App"
        ),
        .testTarget(
            name: "AppTests",
            dependencies: ["Server", "ModelRuntime", "Tooling"],
            path: "Tests"
        ),
    ]
)
