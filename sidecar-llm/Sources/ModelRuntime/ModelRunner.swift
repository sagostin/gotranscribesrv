import CoreML
import Foundation
@preconcurrency import Generation
@preconcurrency import Models
@preconcurrency import Tokenizers

/// Non-Sendable model + tokenizer pair. Only ever touched from its owning
/// `ModelRunner` actor, hence `@unchecked Sendable`.
public final class ModelContext: @unchecked Sendable {
    public let model: any ChatModel
    public let tokenizer: any Tokenizer
    public let entry: ModelRegistryEntry

    public init(model: any ChatModel, tokenizer: any Tokenizer, entry: ModelRegistryEntry) {
        self.model = model
        self.tokenizer = tokenizer
        self.entry = entry
    }
}

/// Serializes all inference for one loaded model.
public actor ModelRunner {
    public let id: String
    let context: ModelContext

    public init(id: String, context: ModelContext) {
        self.id = id
        self.context = context
    }

    /// Render chat messages (+ optional tool schemas as a JSON array, OpenAI function format)
    /// into prompt tokens.
    ///
    /// With tools, we always use the hand-rolled Mistral v0.3 format: the Jinja package
    /// used by swift-transformers silently drops the `[AVAILABLE_TOOLS]` block from the
    /// official template. History containing tool calls/results also goes through the
    /// manual renderer, which matches the official template exactly.
    public func applyChat(messages: [ChatMessage], toolsJSON: Data?) throws -> [Int] {
        let tools: [[String: Any]]? = try toolsJSON.flatMap { data in
            guard data.isEmpty == false else { return nil }
            return try JSONSerialization.jsonObject(with: data) as? [[String: Any]]
        }
        let hasToolHistory = messages.contains { $0.toolCalls != nil || $0.role == "tool" }
        if let tools, !tools.isEmpty {
            return Self.mistralPromptTokens(messages: messages, tools: tools, tokenizer: context.tokenizer)
        }
        if hasToolHistory {
            return Self.mistralPromptTokens(messages: messages, tools: nil, tokenizer: context.tokenizer)
        }
        do {
            let pairs: [[String: any Sendable]] = messages.map {
                ["role": $0.role, "content": $0.content ?? ""]
            }
            return try context.tokenizer.applyChatTemplate(
                messages: pairs,
                chatTemplate: nil,
                addGenerationPrompt: true,
                truncation: false,
                maxLength: nil,
                tools: nil
            )
        } catch {
            return Self.mistralPromptTokens(messages: messages, tools: nil, tokenizer: context.tokenizer)
        }
    }

    /// Generate from prompt tokens. `onDelta` receives decoded text deltas of the
    /// generated (non-prompt) part. Returns the full token sequence (prompt + generated).
    public func generate(
        tokens promptTokens: [Int],
        maxNewTokens: Int,
        temperature: Double = 0,
        topK: Int = 50,
        onDelta: (@Sendable (String) -> Void)? = nil
    ) async throws -> [Int] {
        let model = context.model
        let tokenizer = context.tokenizer

        guard promptTokens.count < model.maxContextLength else {
            throw ModelError.promptTooLong(model: id)
        }
        let cappedNewTokens = min(maxNewTokens, model.maxContextLength - promptTokens.count)

        await model.resetState()

        var config = GenerationConfig(maxNewTokens: cappedNewTokens)
        config.maxLength = promptTokens.count + cappedNewTokens
        config.doSample = temperature > 0
        config.temperature = temperature > 0 ? Float(temperature) : 1.0
        config.topK = topK
        config.eosTokenId = tokenizer.eosTokenId
        config.bosTokenId = tokenizer.bosTokenId
        config.padTokenId = tokenizer.eosTokenId

        var previous = ""
        let output = await model.generate(
            config: config,
            tokens: promptTokens,
            model: model.callAsFunction
        ) { outputTokens in
            guard let onDelta else { return }
            let generated = Array(outputTokens.dropFirst(promptTokens.count))
            let full = tokenizer.decode(tokens: generated)
            let delta: String
            if full.hasPrefix(previous) {
                delta = String(full.dropFirst(previous.count))
            } else {
                delta = full
            }
            previous = full
            if !delta.isEmpty { onDelta(delta) }
        }
        return output
    }

    /// Tokens that append tool results after an assistant tool-call turn (Mistral format):
    /// `</s>[TOOL_RESULTS]...[/TOOL_RESULTS]`. Special-token strings are mapped to their
    /// ids by the tokenizer's added-token handling.
    public func toolResultContinuation(resultsJSON: String) -> [Int] {
        var tokens: [Int] = []
        if let eos = context.tokenizer.eosTokenId {
            tokens.append(eos)
        }
        tokens += context.tokenizer.encode(
            text: "[TOOL_RESULTS]\(resultsJSON)[/TOOL_RESULTS]",
            addSpecialTokens: false
        )
        return tokens
    }

    public func decode(tokens: [Int]) -> String {
        context.tokenizer.decode(tokens: tokens)
    }

    /// Raw prompt tokens (no chat template) for /v1/completions.
    public func encodeRaw(_ prompt: String) -> [Int] {
        context.tokenizer.encode(text: prompt)
    }

    /// Prime ANE compilation / state allocation so the first real request is fast.
    public func warmup() async {
        let tokens = context.tokenizer.encode(text: "Hello")
        _ = try? await generate(tokens: tokens, maxNewTokens: 1)
    }

    /// Mistral v0.3 instruct format, matching the official chat template exactly:
    /// `<s>` + turns, system merged into the LAST user message,
    /// `[AVAILABLE_TOOLS] [...][/AVAILABLE_TOOLS]` right before the last `[INST]`,
    /// assistant tool calls as `[TOOL_CALLS] [...]</s>`, and tool results as
    /// `[TOOL_RESULTS] {"content": ..., "call_id": "..."}[/TOOL_RESULTS]`.
    static func mistralPromptTokens(
        messages: [ChatMessage],
        tools: [[String: Any]]?,
        tokenizer: any Tokenizer
    ) -> [Int] {
        var tokens: [Int] = []
        if let bos = tokenizer.bosTokenId {
            tokens.append(bos)
        }
        tokens += tokenizer.encode(
            text: mistralPromptText(messages: messages, tools: tools),
            addSpecialTokens: false
        )
        return tokens
    }

    /// The full prompt string in official Mistral v0.3 format.
    static func mistralPromptText(messages: [ChatMessage], tools: [[String: Any]]?) -> String {
        var system = ""
        var turns: [ChatMessage] = []
        for message in messages {
            if message.role == "system" {
                system += (system.isEmpty ? "" : "\n\n") + (message.content ?? "")
            } else {
                turns.append(message)
            }
        }
        let lastUserIndex = turns.lastIndex(where: { $0.role == "user" })

        var text = ""
        for (index, turn) in turns.enumerated() {
            switch turn.role {
            case "user":
                if index == lastUserIndex, let tools, !tools.isEmpty {
                    text += "[AVAILABLE_TOOLS] \(toolsJSONArray(tools))[/AVAILABLE_TOOLS]"
                }
                var content = turn.content ?? ""
                if index == lastUserIndex && !system.isEmpty {
                    content = system + "\n\n" + content
                }
                text += "[INST] \(content)[/INST]"
            case "assistant":
                if let toolCalls = turn.toolCalls, !toolCalls.isEmpty {
                    text += toolCallsBlock(toolCalls) + "</s>"
                } else {
                    let content = (turn.content ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
                    text += " " + content + "</s>"
                }
            case "tool":
                text += toolResultsBlock(turn)
            default:
                text += "[INST] \(turn.content ?? "")[/INST]"
            }
        }
        return text
    }

    /// `[TOOL_CALLS] [{"name": ..., "arguments": ..., "id": "XXXXXXXXX"}]`
    static func toolCallsBlock(_ calls: [ChatToolCall]) -> String {
        let parts = calls.map { call in
            "{\"name\": \(jsonString(call.name)), \"arguments\": \(call.argumentsJSON), \"id\": \"\(normalizedToolCallID(call.id))\"}"
        }
        return "[TOOL_CALLS] [\(parts.joined(separator: ", "))]"
    }

    /// `[TOOL_RESULTS] {"content": <raw-if-json-else-string>, "call_id": "..."}[/TOOL_RESULTS]`
    static func toolResultsBlock(_ message: ChatMessage) -> String {
        let content = message.content ?? ""
        let rendered: String
        if let data = content.data(using: .utf8),
           (try? JSONSerialization.jsonObject(with: data, options: [.allowFragments])) != nil
        {
            rendered = content // already a JSON value; template inserts raw
        } else {
            rendered = jsonString(content)
        }
        let callID = normalizedToolCallID(message.toolCallID ?? "")
        return "[TOOL_RESULTS] {\"content\": \(rendered), \"call_id\": \"\(callID)\"}[/TOOL_RESULTS]"
    }

    /// `[{"type": "function", "function": {"name": ..., "description": ..., "parameters": {...}}}]`
    static func toolsJSONArray(_ tools: [[String: Any]]) -> String {
        var parts: [String] = []
        for tool in tools {
            guard let function = tool["function"] as? [String: Any] else { continue }
            var fields: [String] = []
            for (key, value) in function.sorted(by: { $0.key < $1.key }) {
                if let string = value as? String {
                    fields.append("\"\(key)\": \(Self.jsonString(string))")
                } else if let data = try? JSONSerialization.data(withJSONObject: value),
                          let json = String(data: data, encoding: .utf8)
                {
                    fields.append("\"\(key)\": \(json)")
                }
            }
            parts.append("{\"type\": \"function\", \"function\": {\(fields.joined(separator: ", "))}}")
        }
        return "[\(parts.joined(separator: ", "))]"
    }

    static func jsonString(_ string: String) -> String {
        guard let data = try? JSONEncoder().encode(string),
              let json = String(data: data, encoding: .utf8)
        else { return "\"\"" }
        return json
    }
}
