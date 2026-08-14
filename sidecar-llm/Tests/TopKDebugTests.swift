import CoreML
import XCTest
@testable import ModelRuntime

/// Debug the top-k adapter against the real compiled qwen model.
final class TopKDebugTests: XCTestCase {
    func testInspectTopKOutputs() async throws {
        let base = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        let url = base.appending(path: "Models/compiled/qwen3-1.7b-w8.mlmodelc")
        guard FileManager.default.fileExists(atPath: url.path) else {
            throw XCTSkip("qwen model not compiled yet")
        }
        let config = MLModelConfiguration()
        config.computeUnits = .cpuOnly
        let model = try MLModel(contentsOf: url, configuration: config)
        let desc = model.modelDescription
        print("INPUTS:")
        for (name, d) in desc.inputDescriptionsByName {
            print("  \(name): \(d.multiArrayConstraint?.dataType.rawValue ?? -1) shape=\(d.multiArrayConstraint?.shape ?? [])")
        }
        print("OUTPUTS:")
        for (name, d) in desc.outputDescriptionsByName {
            print("  \(name): \(d.multiArrayConstraint?.dataType.rawValue ?? -1) shape=\(d.multiArrayConstraint?.shape ?? [])")
        }
        print("STATES:", desc.stateDescriptionsByName.keys.sorted())

        // Run one prediction on a few tokens.
        let state = model.makeState()
        let tokens: [Int32] = [2, 1234, 5678]
        var inputs: [String: MLTensor] = ["inputIds": MLTensor(tokens).expandingShape(at: 0)]
        if desc.inputDescriptionsByName["causalMask"] != nil {
            inputs["causalMask"] = MLTensor(zeros: [1, 1, 1, tokens.count + 1], scalarType: Float16.self)
        }
        let outputs = try await model.prediction(from: inputs, using: state)
        for (name, tensor) in outputs {
            if name.contains("indic") || name.contains("Indic") || name.contains("ids") {
                let values = await tensor.shapedArray(of: Int32.self).scalars
                print("OUT \(name): shape=\(tensor.shape) first=\(values.prefix(8))")
            } else {
                let values = await tensor.shapedArray(of: Float16.self).scalars
                print("OUT \(name): shape=\(tensor.shape) first=\(values.prefix(8))")
            }
        }
    }
}
