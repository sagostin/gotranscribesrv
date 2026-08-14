// swift-tools-version:6.0
import PackageDescription
import Foundation

// text-processing-rs — Rust port of NeMo ITN/TN.
// Vendored under Vendor/text-processing-rs. The static lib is built by the
// Makefile target `itn-build` and is OPTIONAL — FluidAudio's TextNormalizer
// is a no-op when the libnemo_text_processing symbols are not present
// (dlsym returns NULL → passthrough).
//
// To enable real NeMo ITN, run `make itn-build` once after cloning.
//   - macOS arm64: produces Vendor/text-processing-rs/target/aarch64-apple-darwin/release/libtext_processing_rs.a
//   - macOS x86_64: produces Vendor/text-processing-rs/target/x86_64-apple-darwin/release/libtext_processing_rs.a
//
// SwiftPM links the static lib via unsafeFlags below. The vendored lib is
// statically linked into the sidecar binary, so dlsym(handle, "nemo_normalize")
// in TextNormalizer.swift resolves the symbols from the main executable.
let itnLibSubpath: String
#if arch(arm64)
itnLibSubpath = "Vendor/text-processing-rs/target/aarch64-apple-darwin/release"
#elseif arch(x86_64)
itnLibSubpath = "Vendor/text-processing-rs/target/x86_64-apple-darwin/release"
#else
itnLibSubpath = ""
#endif

// Conditionally add the linker flags only if the static library has been
// built. This makes the link OPTIONAL: the sidecar still builds and runs
// without the lib (passthrough mode), and gains real NeMo ITN once
// `make itn-build` has been run.
var itnLinkerFlags: [String] = []
var itnLinkerSettings: [LinkerSetting] = []
if !itnLibSubpath.isEmpty {
    let pkgRoot = Context.packageDirectory
    let itnLibPath = URL(fileURLWithPath: itnLibSubpath, relativeTo: URL(fileURLWithPath: pkgRoot)).path
    let itnLibFile = "\(itnLibPath)/libtext_processing_rs.a"
    if FileManager.default.fileExists(atPath: itnLibFile) {
        itnLinkerFlags = [
            "-L", itnLibPath,
            "-ltext_processing_rs",
            "-lc++",
        ]
        // The FluidAudio TextNormalizer does dlopen(nil, RTLD_NOW) +
        // dlsym(handle, "nemo_normalize") to resolve the FFI entry points.
        // Statically-linked symbols in the main executable are NOT visible
        // to dlsym on macOS unless explicitly exported. We export every
        // nemo_* symbol present in the v0.2.2 static lib so dlsym finds
        // them at runtime. -force_load keeps the nemo_* object files alive
        // so they aren't dead-stripped.
        //
        // Keep in sync with `nm -gU libtext_processing_rs.a | grep "T _nemo_"`.
        let nemoSymbols = [
            "nemo_add_rule",
            "nemo_clear_rules",
            "nemo_free_string",
            "nemo_normalize",
            "nemo_normalize_sentence",
            "nemo_normalize_sentence_with_options",
            "nemo_normalize_with_options",
            "nemo_remove_rule",
            "nemo_rule_count",
            "nemo_tn_normalize",
            "nemo_tn_normalize_lang",
            "nemo_tn_normalize_sentence",
            "nemo_tn_normalize_sentence_lang",
            "nemo_tn_normalize_sentence_with_max_span",
            "nemo_tn_normalize_sentence_with_max_span_lang",
            "nemo_version",
        ]
        var exportArgs: [String] = ["-Xlinker", "-force_load", "-Xlinker", itnLibFile]
        for sym in nemoSymbols {
            exportArgs += ["-Xlinker", "-exported_symbol", "-Xlinker", "_\(sym)"]
        }
        itnLinkerSettings = [.unsafeFlags(exportArgs)]
    }
}

let package = Package(
    name: "SidecarAudio",
    platforms: [
        .macOS(.v14),
    ],
    dependencies: [
        // FluidAudio — CoreML-optimized ASR, VAD, Diarization, TTS
        .package(url: "https://github.com/FluidInference/FluidAudio.git", exact: "0.15.5"),
        // Vapor — HTTP server with WebSocket, multipart, JSON support
        .package(url: "https://github.com/vapor/vapor.git", from: "4.99.0"),
    ],
    targets: [
        .target(
            // Shared library: pure-Swift ITN helpers (e.g. ITNPreprocessor)
            // consumed by both the executable Server and the test target.
            // Kept separate from Server so the test target can import the
            // symbols without depending on the executable target's
            // dynamic-linking setup.
            name: "ITNHelpers",
            dependencies: [
                .product(name: "FluidAudio", package: "FluidAudio"),
            ],
            path: "Sources/ITNHelpers"
        ),
        .executableTarget(
            name: "Server",
            dependencies: [
                .product(name: "FluidAudio", package: "FluidAudio"),
                .product(name: "Vapor", package: "vapor"),
                "ITNHelpers",
            ],
            path: "Sources/Server",
            swiftSettings: itnLinkerFlags.isEmpty ? [] : [
                // Pass the lib search path + lib name + libstdc++ to the
                // driver. -Xlinker below handles -force_load for the linker.
                .unsafeFlags(itnLinkerFlags)
            ],
            linkerSettings: itnLinkerSettings
        ),
        .testTarget(
            name: "TextNormalizerTests",
            dependencies: [
                .product(name: "FluidAudio", package: "FluidAudio"),
                "ITNHelpers",
            ],
            path: "Tests/TextNormalizerTests",
            // The Server target's linkerSettings aren't applied to test
            // bundles — tests run against a separate .xctest binary. Mirror
            // the same flags here so the test target can dlopen() the
            // nemo_* symbols from text-processing-rs.
            swiftSettings: itnLinkerFlags.isEmpty ? [] : [.unsafeFlags(itnLinkerFlags)],
            linkerSettings: itnLinkerSettings
        ),
    ]
)
