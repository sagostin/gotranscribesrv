import Foundation
@preconcurrency import ExternalRuntime
import ModelRuntime
import Vapor

// MARK: - Anthropic request DTOs

struct AnthropicRequest: Content {
    var model: String
    var maxTokens: Int?
    var system: JSONValue?
    var messages: [AnthropicMessage]
    var tools: [AnthropicTool]?
    var toolChoice: JSONValue?
    var stream: Bool?
    var temperature: Double?
    var topK: Int?
    var stopSequences: [String]?

    enum CodingKeys: String, CodingKey {
        case model, system, messages, tools, stream, temperature
        case maxTokens = "max_tokens"
        case toolChoice = "tool_choice"
        case topK = "top_k"
        case stopSequences = "stop_sequences"
    }

    var toolChoiceIsNone: Bool {
        if case .object(let object) = toolChoice,
           case .string(let type)? = object["type"] { return type == "none" }
        return false
    }

    /// Anthropic tool schemas converted to the internal OpenAI-function shape
    /// the prompt renderer consumes.
    var toolsJSON: Data? {
        guard let tools, !tools.isEmpty else { return nil }
        let array: [[String: Any]] = tools.map { tool in
            [
                "type": "function",
                "function": [
                    "name": tool.name,
                    "description": tool.description ?? "",
                    "parameters": tool.inputSchema?.anyValue ?? ["type": "object", "properties": [String: Any]()],
                ],
            ]
        }
        return try? JSONSerialization.data(withJSONObject: array)
    }
}

struct AnthropicMessage: Content {
    var role: String
    var content: JSONValue
}

struct AnthropicTool: Content {
    var name: String
    var description: String?
    var inputSchema: JSONValue?

    enum CodingKeys: String, CodingKey {
        case name, description
        case inputSchema = "input_schema"
    }
}

// MARK: - Mapping

enum AnthropicMapper {
    /// Anthropic system + messages -> internal chat history.
    static func toChatMessages(system: JSONValue?, messages: [AnthropicMessage]) -> [ChatMessage] {
        var result: [ChatMessage] = []
        if let systemText = renderText(system), !systemText.isEmpty {
            result.append(ChatMessage(role: "system", content: systemText))
        }
        for message in messages {
            switch message.content {
            case .string(let text):
                result.append(ChatMessage(role: message.role, content: text))
            case .array(let blocks):
                result.append(contentsOf: mapBlocks(role: message.role, blocks: blocks))
            default:
                result.append(ChatMessage(role: message.role, content: ""))
            }
        }
        return result
    }

    private static func mapBlocks(role: String, blocks: [JSONValue]) -> [ChatMessage] {
        var result: [ChatMessage] = []
        var textParts: [String] = []
        var toolCalls: [ChatToolCall] = []

        func flushAssistant() {
            if role == "assistant" && (!textParts.isEmpty || !toolCalls.isEmpty) {
                result.append(ChatMessage(
                    role: "assistant",
                    content: textParts.joined(separator: "\n"),
                    toolCalls: toolCalls.isEmpty ? nil : toolCalls))
                textParts = []
                toolCalls = []
            }
        }

        for block in blocks {
            guard case .object(let object) = block,
                  case .string(let type)? = object["type"] else { continue }
            switch type {
            case "text":
                if case .string(let text)? = object["text"] {
                    if role == "assistant" {
                        textParts.append(text)
                    } else {
                        flushAssistant()
                        result.append(ChatMessage(role: role, content: text))
                    }
                }
            case "tool_use":
                let id = object["id"]?.stringValue ?? ""
                let name = object["name"]?.stringValue ?? ""
                let input = object["input"]?.jsonString ?? "{}"
                toolCalls.append(ChatToolCall(id: id, name: name, argumentsJSON: input))
            case "tool_result":
                flushAssistant()
                let toolUseID = object["tool_use_id"]?.stringValue ?? ""
                result.append(ChatMessage(
                    role: "tool",
                    content: renderText(object["content"]) ?? "",
                    toolCallID: toolUseID))
            default:
                break // images etc. unsupported (no vision models yet)
            }
        }
        flushAssistant()
        return result
    }

    /// Text from a string, or from an array of text blocks.
    static func renderText(_ value: JSONValue?) -> String? {
        guard let value else { return nil }
        switch value {
        case .string(let text): return text
        case .array(let blocks):
            return blocks.compactMap { block -> String? in
                guard case .object(let object) = block,
                      case .string("text")? = object["type"],
                      case .string(let text)? = object["text"] else { return nil }
                return text
            }.joined(separator: "\n")
        default: return value.jsonString
        }
    }
}

extension JSONValue {
    var stringValue: String? {
        if case .string(let value) = self { return value }
        return nil
    }
}

// MARK: - SSE frames

enum AnthropicSSE {
    static func event(_ name: String, _ payload: [String: Any]) -> String {
        guard let data = try? JSONSerialization.data(withJSONObject: payload),
              let json = String(data: data, encoding: .utf8)
        else { return "" }
        return "event: \(name)\ndata: \(json)\n\n"
    }

    static func messageStart(id: String, model: String, inputTokens: Int) -> String {
        event("message_start", [
            "type": "message_start",
            "message": [
                "id": id,
                "type": "message",
                "role": "assistant",
                "model": model,
                "content": [Any](),
                "stop_reason": NSNull(),
                "stop_sequence": NSNull(),
                "usage": ["input_tokens": inputTokens, "output_tokens": 0],
            ],
        ])
    }

    static func textBlockStart(index: Int) -> String {
        event("content_block_start", [
            "type": "content_block_start",
            "index": index,
            "content_block": ["type": "text", "text": ""],
        ])
    }

    static func textDelta(index: Int, _ text: String) -> String {
        event("content_block_delta", [
            "type": "content_block_delta",
            "index": index,
            "delta": ["type": "text_delta", "text": text],
        ])
    }

    static func toolUseStart(index: Int, id: String, name: String) -> String {
        event("content_block_start", [
            "type": "content_block_start",
            "index": index,
            "content_block": ["type": "tool_use", "id": id, "name": name, "input": [String: Any]()],
        ])
    }

    static func toolInputDelta(index: Int, partialJSON: String) -> String {
        event("content_block_delta", [
            "type": "content_block_delta",
            "index": index,
            "delta": ["type": "input_json_delta", "partial_json": partialJSON],
        ])
    }

    static func blockStop(index: Int) -> String {
        event("content_block_stop", ["type": "content_block_stop", "index": index])
    }

    static func messageDelta(stopReason: String, outputTokens: Int) -> String {
        event("message_delta", [
            "type": "message_delta",
            "delta": ["stop_reason": stopReason, "stop_sequence": NSNull()],
            "usage": ["output_tokens": outputTokens],
        ])
    }

    static let messageStop = "event: message_stop\ndata: {\"type\": \"message_stop\"}\n\n"
}

// MARK: - Route

func anthropicRoutes(_ app: Application, context: ServerContext) {
    app.post("v1", "messages") { req async throws -> Response in
        let anthropicRequest = try req.content.decode(AnthropicRequest.self)
        // Dispatch coreml-llm entries (e.g. gemma-4-E2B) through the
        // external backend, with gemma-4 tool-calling support.
        if let entry = await context.manager.entries.first(where: { $0.id == anthropicRequest.model }),
           entry.runtime == .coremlLLM
        {
            return try await handleExternalAnthropicMessage(anthropicRequest, context: context)
        }
        return try await handleAnthropicMessage(anthropicRequest, context: context)
    }
}

func handleExternalAnthropicMessage(
    _ anthropicRequest: AnthropicRequest,
    context: ServerContext
) async throws -> Response {
    guard let clm = context.coremlLLM else { return coremlLLMUnavailable() }
    let entry = await context.manager.entries.first { $0.id == anthropicRequest.model }
    let maxNewTokens = anthropicRequest.maxTokens ?? entry?.maxNewTokens ?? 512
    let messages = AnthropicMapper.toChatMessages(
        system: anthropicRequest.system, messages: anthropicRequest.messages)
    let toolsJSON = anthropicRequest.toolChoiceIsNone ? nil : anthropicRequest.toolsJSON
    let messageID = newAnthropicID(prefix: "msg_")
    let modelName = anthropicRequest.model

    if anthropicRequest.stream == true {
        return streamingResponse { emit in
            try await runExternalAnthropicStream(
                emit: emit,
                clm: clm,
                modelName: modelName,
                messages: messages,
                toolsJSON: toolsJSON,
                maxNewTokens: maxNewTokens,
                messageID: messageID)
        }
    }

    let (text, calls) = try await runExternalAnthropicOnce(
        clm: clm, modelName: modelName, messages: messages, toolsJSON: toolsJSON,
        maxNewTokens: maxNewTokens)

    var content: [[String: Any]] = []
    var stopReason = "end_turn"
    if calls.isEmpty {
        content.append(["type": "text", "text": text])
    } else {
        for call in calls {
            let inputObject = ((try? JSONSerialization.jsonObject(with: Data(call.argumentsJSON.utf8))) as? [String: Any]) ?? [:]
            content.append([
                "type": "tool_use", "id": call.id, "name": call.name, "input": inputObject,
            ])
        }
        stopReason = "tool_use"
    }
    return jsonResponse([
        "id": messageID,
        "type": "message",
        "role": "assistant",
        "model": modelName,
        "content": content,
        "stop_reason": stopReason,
        "stop_sequence": NSNull(),
        "usage": ["input_tokens": 0, "output_tokens": 0],
    ])
}

struct ExternalAnthropicOutcome: Sendable {
    var text: String
    var calls: [ChatToolCall]
}

func runExternalAnthropicOnce(
    clm: CoreMLLLMManager,
    modelName: String,
    messages: [ChatMessage],
    toolsJSON: Data?,
    maxNewTokens: Int
) async throws -> (String, [ChatToolCall]) {
    let model = try await clm.model(for: modelName)
    var emittedText: String?
    var calls: [ChatToolCall] = []
    let raw = try await model.generate(
        messages: messages, toolsJSON: toolsJSON, maxTokens: maxNewTokens) { _ in }
    let (cleanText, parsedCalls) = Gemma4Tools.parse(raw)
    if !parsedCalls.isEmpty {
        for parsed in parsedCalls {
            calls.append(ChatToolCall(
                id: "toolu_" + newToolCallID(),
                name: parsed.name,
                argumentsJSON: parsed.argumentsJSON))
        }
    } else {
        emittedText = cleanText
    }
    return (emittedText ?? "", calls)
}

func runExternalAnthropicStream(
    emit: @escaping @Sendable (String) -> Void,
    clm: CoreMLLLMManager,
    modelName: String,
    messages: [ChatMessage],
    toolsJSON: Data?,
    maxNewTokens: Int,
    messageID: String
) async throws {
    let (text, calls) = try await runExternalAnthropicOnce(
        clm: clm, modelName: modelName, messages: messages, toolsJSON: toolsJSON,
        maxNewTokens: maxNewTokens)
    emit(AnthropicSSE.messageStart(id: messageID, model: modelName, inputTokens: 0))
    var blockIndex = 0
    if !calls.isEmpty {
        for call in calls {
            emit(AnthropicSSE.toolUseStart(index: blockIndex, id: call.id, name: call.name))
            emit(AnthropicSSE.toolInputDelta(index: blockIndex, partialJSON: call.argumentsJSON))
            emit(AnthropicSSE.blockStop(index: blockIndex))
            blockIndex += 1
        }
        emit(AnthropicSSE.messageDelta(stopReason: "tool_use", outputTokens: 0))
    } else {
        emit(AnthropicSSE.textBlockStart(index: blockIndex))
        emit(AnthropicSSE.textDelta(index: blockIndex, text))
        emit(AnthropicSSE.blockStop(index: blockIndex))
        emit(AnthropicSSE.messageDelta(stopReason: "end_turn", outputTokens: 0))
    }
    emit(AnthropicSSE.messageStop)
}

func handleAnthropicMessage(
    _ anthropicRequest: AnthropicRequest,
    context: ServerContext
) async throws -> Response {
    let runner: ModelRunner
    do {
        runner = try await context.manager.runner(for: anthropicRequest.model)
    } catch let error as ModelError {
        return errorResponse(error.httpStatus, message: error.description, type: "invalid_request_error")
    }

    let entry = await context.manager.entries.first { $0.id == anthropicRequest.model }
    let maxNewTokens = anthropicRequest.maxTokens ?? entry?.maxNewTokens ?? 512
    let messages = AnthropicMapper.toChatMessages(
        system: anthropicRequest.system, messages: anthropicRequest.messages)
    let toolsJSON = anthropicRequest.toolChoiceIsNone ? nil : anthropicRequest.toolsJSON
    let messageID = newAnthropicID(prefix: "msg_")

    if anthropicRequest.stream == true {
        let model = anthropicRequest.model
        let temperature = anthropicRequest.temperature ?? 0
        let topK = anthropicRequest.topK ?? 50
        return streamingResponse { emit in
            try await anthropicStream(
                emit: emit, runner: runner, messages: messages, toolsJSON: toolsJSON,
                maxNewTokens: maxNewTokens, temperature: temperature, topK: topK,
                messageID: messageID, model: model)
        }
    }

        let result = try await runChatRound(
            runner: runner,
            messages: messages,
            toolsJSON: toolsJSON,
            maxNewTokens: maxNewTokens,
            temperature: anthropicRequest.temperature ?? 0,
            topK: anthropicRequest.topK ?? 50,
            idPrefix: "toolu_"
        )

        var content: [[String: Any]] = []
        var stopReason = result.hitMaxTokens ? "max_tokens" : "end_turn"
        switch result.outcome {
        case .text(let text):
            content.append(["type": "text", "text": text])
        case .toolCalls(let text, let calls):
            let prefix = textBeforeToolMarker(text)
            if !prefix.isEmpty {
                content.append(["type": "text", "text": prefix])
            }
            for call in calls {
                let inputObject = ((try? JSONSerialization.jsonObject(
                    with: Data(call.argumentsJSON.utf8))) as? [String: Any]) ?? [:]
                let block: [String: Any] = [
                    "type": "tool_use", "id": call.id, "name": call.name, "input": inputObject,
                ]
                content.append(block)
            }
            stopReason = "tool_use"
        }

        let usage: [String: Any] = [
            "input_tokens": result.promptTokens,
            "output_tokens": result.completionTokens,
        ]
        let payload: [String: Any] = [
            "id": messageID,
            "type": "message",
            "role": "assistant",
            "model": anthropicRequest.model,
            "content": content,
            "stop_reason": stopReason,
            "stop_sequence": NSNull(),
            "usage": usage,
        ]
        return jsonResponse(payload)
}

/// Text preceding the [TOOL_CALLS] marker, if the model said something first.
func textBeforeToolMarker(_ text: String) -> String {
    guard let range = text.range(of: "[TOOL_CALLS]") else { return "" }
    return text[..<range.lowerBound].trimmingCharacters(in: .whitespacesAndNewlines)
}

/// Anthropic SSE event sequence for one message.
func anthropicStream(
    emit: @escaping @Sendable (String) -> Void,
    runner: ModelRunner,
    messages: [ChatMessage],
    toolsJSON: Data?,
    maxNewTokens: Int,
    temperature: Double,
    topK: Int,
    messageID: String,
    model: String
) async throws {
    let liveText = toolsJSON == nil
    // message_start carries the real input-token count: the onPromptTokens
    // callback fires after prompt tokenization but before any generation
    // delta, so event ordering (message_start → content_block_start →
    // deltas) is preserved.
    let onPromptTokens: @Sendable (Int) -> Void = { count in
        emit(AnthropicSSE.messageStart(id: messageID, model: model, inputTokens: count))
        if liveText { emit(AnthropicSSE.textBlockStart(index: 0)) }
    }

    var onText: (@Sendable (String) -> Void)?
    if liveText {
        onText = { delta in emit(AnthropicSSE.textDelta(index: 0, delta)) }
    }
    let result = try await runChatRound(
        runner: runner,
        messages: messages,
        toolsJSON: toolsJSON,
        maxNewTokens: maxNewTokens,
        temperature: temperature,
        topK: topK,
        idPrefix: "toolu_",
        onText: onText,
        onPromptTokens: onPromptTokens
    )

    switch result.outcome {
    case .text(let text):
        if !liveText {
            emit(AnthropicSSE.textBlockStart(index: 0))
            if !text.isEmpty { emit(AnthropicSSE.textDelta(index: 0, text)) }
        }
        emit(AnthropicSSE.blockStop(index: 0))
        emit(AnthropicSSE.messageDelta(
            stopReason: result.hitMaxTokens ? "max_tokens" : "end_turn",
            outputTokens: result.completionTokens))
    case .toolCalls(let text, let calls):
        var blockIndex = 0
        let prefix = textBeforeToolMarker(text)
        if !prefix.isEmpty {
            emit(AnthropicSSE.textBlockStart(index: blockIndex))
            emit(AnthropicSSE.textDelta(index: blockIndex, prefix))
            emit(AnthropicSSE.blockStop(index: blockIndex))
            blockIndex += 1
        }
        for call in calls {
            emit(AnthropicSSE.toolUseStart(index: blockIndex, id: call.id, name: call.name))
            emit(AnthropicSSE.toolInputDelta(index: blockIndex, partialJSON: call.argumentsJSON))
            emit(AnthropicSSE.blockStop(index: blockIndex))
            blockIndex += 1
        }
        emit(AnthropicSSE.messageDelta(
            stopReason: "tool_use", outputTokens: result.completionTokens))
    }
    emit(AnthropicSSE.messageStop)
}
