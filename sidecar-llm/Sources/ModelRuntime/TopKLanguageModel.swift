import CoreML
import Foundation
@preconcurrency import Generation
@preconcurrency import Models
@preconcurrency import Tokenizers

/// Minimal surface ModelRunner needs from a loaded chat model, so alternative
/// model layouts (e.g. top-k outputs) can plug in alongside swift-transformers'
/// LanguageModel.
@preconcurrency public protocol ChatModel: Generation {
    var maxContextLength: Int { get }
    func resetState() async
    func predictNextTokenScores(_ tokens: MLTensor, config: GenerationConfig) async -> MLTensor
}

public extension ChatModel {
    func callAsFunction(_ input: MLTensor, config: GenerationConfig) async -> MLTensor {
        await predictNextTokenScores(input, config: config)
    }
}

/// swift-transformers' LanguageModel already satisfies every requirement.
extension LanguageModel: ChatModel {}

/// Adapter for stateful CoreML LLMs whose graph emits top-k token ids + scores
/// instead of full logits (e.g. groxaxo/qwen3-1.7b-coreml-int8, converted with
/// `--top-k 64`). The top-k result is scattered into a logits-shaped tensor so
/// the standard generation loop (greedy / top-k sampling) works unchanged.
///
/// Note: values are used as-is; if a converter emits probabilities rather than
/// logits, greedy decoding is unaffected and top-k sampling remains sensible.
public final class TopKLanguageModel: ChatModel, @unchecked Sendable {
    public let model: MLModel
    public let maxContextLength: Int
    public let vocabSize: Int

    private let indicesOutput: String
    private let valuesOutput: String
    private let requiresCausalMask: Bool

    private var state: MLState?
    private var prefilling = true

    /// Generation protocol requirement (prompt-string variant). ModelRunner drives
    /// generation via the tokens-based extension method instead.
    public func generate(
        config: GenerationConfig,
        prompt: String,
        model: NextTokenModel,
        tokenizer: any Tokenizer,
        callback: PredictionStringCallback?
    ) async -> String {
        let tokens = tokenizer.encode(text: prompt)
        var generationConfig = config
        generationConfig.maxLength = config.maxNewTokens + tokens.count
        generationConfig.eosTokenId = tokenizer.eosTokenId
        generationConfig.bosTokenId = tokenizer.bosTokenId
        let output = await generate(config: generationConfig, tokens: tokens, model: model) { outputTokens in
            callback?(tokenizer.decode(tokens: outputTokens))
        }
        return tokenizer.decode(tokens: output)
    }

    public init(model: MLModel, vocabSize: Int) throws {
        self.model = model
        self.vocabSize = vocabSize

        // Context range from the inputIds shape constraint.
        guard let input = model.modelDescription.inputDescriptionsByName["inputIds"],
              let shapeConstraint = input.multiArrayConstraint?.shapeConstraint
        else {
            throw ModelError.incompatibleLayout("(topk)", inputs: "no inputIds input")
        }
        var maxContext = 1024
        if shapeConstraint.type == .range,
           let ranges = input.multiArrayConstraint?.shapeConstraint.sizeRangeForDimension,
           let last = ranges.last as? NSRange
        {
            maxContext = last.length
        }
        self.maxContextLength = maxContext

        // Detect the two top-k outputs: one integer (indices), one float (scores).
        var indicesName: String?
        var valuesName: String?
        for (name, description) in model.modelDescription.outputDescriptionsByName {
            switch description.multiArrayConstraint?.dataType {
            case .int32: indicesName = name
            case .float16, .float32, .double: valuesName = name
            default: break
            }
        }
        guard let indicesName, let valuesName else {
            let outputs = model.modelDescription.outputDescriptionsByName.keys.sorted().joined(separator: ", ")
            throw ModelError.incompatibleLayout("(topk)", inputs: "outputs: \(outputs)")
        }
        self.indicesOutput = indicesName
        self.valuesOutput = valuesName

        self.requiresCausalMask =
            model.modelDescription.inputDescriptionsByName["causalMask"] != nil

        guard model.modelDescription.stateDescriptionsByName.isEmpty == false else {
            throw ModelError.incompatibleLayout("(topk)", inputs: "model is not stateful")
        }
        resetStateSync()
    }

    private func resetStateSync() {
        state = model.makeState()
        prefilling = true
    }

    public func resetState() async {
        resetStateSync()
    }

    public func predictNextTokenScores(_ tokens: MLTensor, config _: GenerationConfig) async -> MLTensor {
        let tokenCount = tokens.shape[1]
        guard let state else { fatalError("TopKLanguageModel: state not initialized") }

        let inputIds = prefilling ? tokens : tokens[nil, -1].expandingShape(at: 0)
        prefilling = false

        var inputDictionary: [String: MLTensor] = ["inputIds": inputIds]
        if requiresCausalMask {
            inputDictionary["causalMask"] = MLTensor(
                zeros: [1, 1, 1, tokenCount + 1], scalarType: Float16.self)
        }
        let outputs = try! await model.prediction(from: inputDictionary, using: state)

        // Scatter top-k results into a logits-shaped [1, 1, vocab] tensor.
        let indices = await outputs[indicesOutput]!.shapedArray(of: Int32.self).scalars
        let values = await outputs[valuesOutput]!.shapedArray(of: Float16.self).scalars
        var scores = [Float](repeating: -1.0e9, count: vocabSize)
        for (offset, index) in indices.enumerated() {
            let i = Int(index)
            if i >= 0 && i < vocabSize {
                scores[i] = Float(values[offset])
            }
        }
        return MLTensor(shape: [1, 1, vocabSize], scalars: scores)
    }
}
