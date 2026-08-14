import Foundation
@preconcurrency import ExternalRuntime
import ModelRuntime
import Vapor

// MARK: - Responses API request DTOs

/// Responses-style tool: flat shape ({type: "function", name, ...}), unlike
/// chat completions where the spec nests under `function`.
struct ResponsesTool: Content {
    var type: String?
    var name: String?
    var description: String?
    var parameters: JSONValue?
    var strict: Bool?
}

/// OpenAI Responses API request (POST /v1/responses). Unknown fields
/// (background, reasoning, text.format, include, …) are ignored — Codable
/// drops them by default.
struct ResponsesRequest: Content {
    var model: String
    /// String shorthand or an array of input items (message / function_call /
    /// function_call_output). The Go gateway prepends stored conversation
    /// history before proxying, so the sidecar only ever sees a flat list.
    var input: JSONValue?
    var instructions: String?
    var tools: [ResponsesTool]?
    var toolChoice: JSONValue?
    var maxOutputTokens: Int?
    var temperature: Double?
    var topP: Double?
    var topK: Int?
    var stream: Bool?
    var store: Bool?
    var metadata: JSONValue?

    enum CodingKeys: String, CodingKey {
        case model, input, instructions, tools, stream, store, metadata, temperature
        case toolChoice = "tool_choice"
        case maxOutputTokens = "max_output_tokens"
        case topP = "top_p"
        case topK = "top_k"
    }

    var toolChoiceIsNone: Bool {
        if case .string(let value) = toolChoice { return value == "none" }
        if case .object(let object) = toolChoice,
           case .string(let type)? = object["type"] { return type == "none" }
        return false
    }

    /// Function tools converted to the internal OpenAI-function shape the
    /// prompt renderer consumes. Built-in tools (web_search, file_search, …)
    /// have no local implementation and are skipped.
    var toolsJSON: Data? {
        guard let tools, !tools.isEmpty else { return nil }
        let array: [[String: Any]] = tools.compactMap { tool in
            guard let name = tool.name,
                  tool.type == nil || tool.type == "function" else { return nil }
            return [
                "type": "function",
                "function": [
                    "name": name,
                    "description": tool.description ?? "",
                    "parameters": tool.parameters?.anyValue ?? [String: Any](),
                ],
            ]
        }
        guard !array.isEmpty else { return nil }
        return try? JSONSerialization.data(withJSONObject: array)
    }
}

// MARK: - Mapping

enum ResponsesMapper {
    enum MapperError: Error, CustomStringConvertible {
        case unsupportedContent(String)

        var description: String {
            switch self {
            case .unsupportedContent(let kind):
                return "\(kind) are not supported by the available models (text-only)"
            }
        }
    }

    /// instructions + input items -> internal chat history.
    static func toChatMessages(instructions: String?, input: JSONValue?) throws -> [ChatMessage] {
        var result: [ChatMessage] = []
        if let instructions, !instructions.isEmpty {
            result.append(ChatMessage(role: "system", content: instructions))
        }
        guard let input else { return result }
        switch input {
        case .string(let text):
            result.append(ChatMessage(role: "user", content: text))
        case .array(let items):
            for item in items {
                try mapItem(item, into: &result)
            }
        default:
            break
        }
        return result
    }

    private static func mapItem(_ item: JSONValue, into result: inout [ChatMessage]) throws {
        guard case .object(let object) = item else { return }
        let type = object["type"]?.stringValue ?? "message"
        switch type {
        case "message":
            let role = mapRole(object["role"]?.stringValue ?? "user")
            let text = try renderContent(object["content"])
            result.append(ChatMessage(role: role, content: text))
        case "function_call":
            let callID = object["call_id"]?.stringValue ?? object["id"]?.stringValue ?? ""
            let name = object["name"]?.stringValue ?? ""
            let args = object["arguments"]?.stringValue ?? "{}"
            result.append(ChatMessage(
                role: "assistant",
                toolCalls: [ChatToolCall(id: callID, name: name, argumentsJSON: args)]))
        case "function_call_output":
            let callID = object["call_id"]?.stringValue ?? ""
            let text = try renderContent(object["output"])
            result.append(ChatMessage(role: "tool", content: text, toolCallID: callID))
        default:
            break // reasoning items etc. can't be replayed into a text model — skip
        }
    }

    private static func mapRole(_ role: String) -> String {
        role == "developer" ? "system" : role
    }

    /// Text from a string shorthand or an array of content parts.
    private static func renderContent(_ value: JSONValue?) throws -> String {
        guard let value else { return "" }
        switch value {
        case .string(let text):
            return text
        case .array(let parts):
            var texts: [String] = []
            for part in parts {
                guard case .object(let object) = part,
                      case .string(let type)? = object["type"] else { continue }
                switch type {
                case "input_text", "output_text":
                    if case .string(let text)? = object["text"] { texts.append(text) }
                case "input_image":
                    throw MapperError.unsupportedContent("Image inputs")
                case "input_file":
                    throw MapperError.unsupportedContent("File inputs")
                default:
                    continue // refusals etc.
                }
            }
            return texts.joined(separator: "\n")
        default:
            return value.jsonString
        }
    }
}

// MARK: - Output items / response object

/// Assistant message output item. `text == nil` yields an empty content array
/// (in_progress frames).
func messageOutputItem(id: String, text: String?, status: String) -> [String: Any] {
    var content: [[String: Any]] = []
    if let text {
        content.append(["type": "output_text", "text": text, "annotations": [Any]()])
    }
    return [
        "id": id,
        "type": "message",
        "status": status,
        "role": "assistant",
        "content": content,
    ]
}

/// Function call output item. `argumentsJSON == nil` yields "" (in_progress frames).
func functionCallOutputItem(
    id: String, callID: String, name: String, argumentsJSON: String?, status: String
) -> [String: Any] {
    [
        "id": id,
        "type": "function_call",
        "status": status,
        "call_id": callID,
        "name": name,
        "arguments": argumentsJSON ?? "",
    ]
}

/// Terminal response object (also embedded in the response.completed event).
func completedResponsePayload(
    id: String, model: String, request: ResponsesRequest,
    items: [[String: Any]], outputText: String,
    promptTokens: Int, completionTokens: Int, hitMaxTokens: Bool
) -> [String: Any] {
    var payload: [String: Any] = [
        "id": id,
        "object": "response",
        "created_at": Int(Date().timeIntervalSince1970),
        "status": hitMaxTokens ? "incomplete" : "completed",
        "model": model,
        "output": items,
        "output_text": outputText,
        "error": NSNull(),
        "usage": [
            "input_tokens": promptTokens,
            "output_tokens": completionTokens,
            "total_tokens": promptTokens + completionTokens,
        ],
        "store": request.store ?? true,
        "metadata": request.metadata?.anyValue ?? [String: Any](),
    ]
    payload["incomplete_details"] = hitMaxTokens ? ["reason": "max_output_tokens"] : NSNull()
    return payload
}

// MARK: - SSE frames

enum ResponsesSSE {
    static func event(_ name: String, _ payload: [String: Any]) -> String {
        guard let data = try? JSONSerialization.data(withJSONObject: payload),
              let json = String(data: data, encoding: .utf8)
        else { return "" }
        return "event: \(name)\ndata: \(json)\n\n"
    }

    /// response.created / response.in_progress — output not populated yet.
    static func responseState(_ type: String, id: String, model: String) -> String {
        event(type, [
            "type": type,
            "response": [
                "id": id,
                "object": "response",
                "created_at": Int(Date().timeIntervalSince1970),
                "status": "in_progress",
                "model": model,
                "output": [Any](),
                "usage": NSNull(),
            ],
        ])
    }

    static func outputItemAdded(outputIndex: Int, item: [String: Any]) -> String {
        event("response.output_item.added", [
            "type": "response.output_item.added", "output_index": outputIndex, "item": item,
        ])
    }

    static func outputItemDone(outputIndex: Int, item: [String: Any]) -> String {
        event("response.output_item.done", [
            "type": "response.output_item.done", "output_index": outputIndex, "item": item,
        ])
    }

    static func contentPartAdded(itemID: String, outputIndex: Int, contentIndex: Int) -> String {
        event("response.content_part.added", [
            "type": "response.content_part.added",
            "item_id": itemID, "output_index": outputIndex, "content_index": contentIndex,
            "part": ["type": "output_text", "text": "", "annotations": [Any]()],
        ])
    }

    static func contentPartDone(itemID: String, outputIndex: Int, contentIndex: Int, text: String) -> String {
        event("response.content_part.done", [
            "type": "response.content_part.done",
            "item_id": itemID, "output_index": outputIndex, "content_index": contentIndex,
            "part": ["type": "output_text", "text": text, "annotations": [Any]()],
        ])
    }

    static func outputTextDelta(itemID: String, outputIndex: Int, contentIndex: Int, delta: String) -> String {
        event("response.output_text.delta", [
            "type": "response.output_text.delta",
            "item_id": itemID, "output_index": outputIndex, "content_index": contentIndex,
            "delta": delta,
        ])
    }

    static func outputTextDone(itemID: String, outputIndex: Int, contentIndex: Int, text: String) -> String {
        event("response.output_text.done", [
            "type": "response.output_text.done",
            "item_id": itemID, "output_index": outputIndex, "content_index": contentIndex,
            "text": text,
        ])
    }

    static func functionCallArgumentsDelta(itemID: String, outputIndex: Int, delta: String) -> String {
        event("response.function_call_arguments.delta", [
            "type": "response.function_call_arguments.delta",
            "item_id": itemID, "output_index": outputIndex, "delta": delta,
        ])
    }

    static func functionCallArgumentsDone(itemID: String, outputIndex: Int, arguments: String) -> String {
        event("response.function_call_arguments.done", [
            "type": "response.function_call_arguments.done",
            "item_id": itemID, "output_index": outputIndex, "arguments": arguments,
        ])
    }

    static func completed(response: [String: Any]) -> String {
        event("response.completed", ["type": "response.completed", "response": response])
    }
}

/// Emits item events + response.completed for an outcome whose deltas were NOT
/// streamed live (tool-enabled rounds buffer; the external backend has no live
/// deltas). Item ids are generated here so the events and the terminal
/// response.completed payload reference the same items.
func emitBufferedResponsesOutcome(
    emit: @escaping @Sendable (String) -> Void,
    responseID: String, model: String, request: ResponsesRequest,
    text: String?, calls: [ChatToolCall],
    promptTokens: Int, completionTokens: Int, hitMaxTokens: Bool
) {
    var outputIndex = 0
    var items: [[String: Any]] = []
    var outputText = ""

    if let text, !text.isEmpty {
        let messageID = newAnthropicID(prefix: "msg_")
        emit(ResponsesSSE.outputItemAdded(
            outputIndex: outputIndex,
            item: messageOutputItem(id: messageID, text: nil, status: "in_progress")))
        emit(ResponsesSSE.contentPartAdded(itemID: messageID, outputIndex: outputIndex, contentIndex: 0))
        emit(ResponsesSSE.outputTextDelta(
            itemID: messageID, outputIndex: outputIndex, contentIndex: 0, delta: text))
        emit(ResponsesSSE.outputTextDone(
            itemID: messageID, outputIndex: outputIndex, contentIndex: 0, text: text))
        emit(ResponsesSSE.contentPartDone(
            itemID: messageID, outputIndex: outputIndex, contentIndex: 0, text: text))
        let item = messageOutputItem(id: messageID, text: text, status: "completed")
        emit(ResponsesSSE.outputItemDone(outputIndex: outputIndex, item: item))
        items.append(item)
        outputText = text
        outputIndex += 1
    }

    for call in calls {
        let itemID = newAnthropicID(prefix: "fc_")
        emit(ResponsesSSE.outputItemAdded(
            outputIndex: outputIndex,
            item: functionCallOutputItem(
                id: itemID, callID: call.id, name: call.name,
                argumentsJSON: nil, status: "in_progress")))
        emit(ResponsesSSE.functionCallArgumentsDelta(
            itemID: itemID, outputIndex: outputIndex, delta: call.argumentsJSON))
        emit(ResponsesSSE.functionCallArgumentsDone(
            itemID: itemID, outputIndex: outputIndex, arguments: call.argumentsJSON))
        let item = functionCallOutputItem(
            id: itemID, callID: call.id, name: call.name,
            argumentsJSON: call.argumentsJSON, status: "completed")
        emit(ResponsesSSE.outputItemDone(outputIndex: outputIndex, item: item))
        items.append(item)
        outputIndex += 1
    }

    emit(ResponsesSSE.completed(response: completedResponsePayload(
        id: responseID, model: model, request: request,
        items: items, outputText: outputText,
        promptTokens: promptTokens, completionTokens: completionTokens,
        hitMaxTokens: hitMaxTokens)))
}

// MARK: - Route

func responsesRoutes(_ app: Application, context: ServerContext) {
    app.post("v1", "responses") { req async throws -> Response in
        let responsesRequest: ResponsesRequest
        do {
            responsesRequest = try req.content.decode(ResponsesRequest.self)
        } catch {
            return errorResponse(
                .badRequest, message: "malformed request body", type: "invalid_request_error")
        }
        guard let entry = await context.manager.entries.first(where: { $0.id == responsesRequest.model }) else {
            return unknownModelResponse(
                responsesRequest.model, entries: await context.manager.entries)
        }

        let messages: [ChatMessage]
        do {
            messages = try ResponsesMapper.toChatMessages(
                instructions: responsesRequest.instructions, input: responsesRequest.input)
        } catch let error as ResponsesMapper.MapperError {
            return errorResponse(.badRequest, message: error.description, type: "invalid_request_error")
        }

        let maxNewTokens = responsesRequest.maxOutputTokens ?? entry.maxNewTokens ?? 512
        let toolsJSON = responsesRequest.toolChoiceIsNone ? nil : responsesRequest.toolsJSON

        if entry.runtime == .coremlLLM {
            return try await handleExternalResponse(
                responsesRequest, context: context,
                messages: messages, toolsJSON: toolsJSON, maxNewTokens: maxNewTokens)
        }
        return try await handleResponse(
            responsesRequest, context: context,
            messages: messages, toolsJSON: toolsJSON, maxNewTokens: maxNewTokens)
    }
}

/// Standard-runtime (in-tree model) Responses handler.
func handleResponse(
    _ responsesRequest: ResponsesRequest,
    context: ServerContext,
    messages: [ChatMessage],
    toolsJSON: Data?,
    maxNewTokens: Int
) async throws -> Response {
    let runner: ModelRunner
    do {
        runner = try await context.manager.runner(for: responsesRequest.model)
    } catch let error as ModelError {
        return errorResponse(error.httpStatus, message: error.description, type: "invalid_request_error")
    }

    let responseID = newAnthropicID(prefix: "resp_")

    if responsesRequest.stream == true {
        let model = responsesRequest.model
        let temperature = responsesRequest.temperature ?? 0
        let topK = responsesRequest.topK ?? 50
        let request = responsesRequest
        return streamingResponse { emit in
            try await responsesStream(
                emit: emit, runner: runner, messages: messages, toolsJSON: toolsJSON,
                maxNewTokens: maxNewTokens, temperature: temperature, topK: topK,
                responseID: responseID, model: model, request: request)
        }
    }

    let result = try await runChatRound(
        runner: runner,
        messages: messages,
        toolsJSON: toolsJSON,
        maxNewTokens: maxNewTokens,
        temperature: responsesRequest.temperature ?? 0,
        topK: responsesRequest.topK ?? 50,
        idPrefix: "call_"
    )

    var items: [[String: Any]] = []
    var outputText = ""
    switch result.outcome {
    case .text(let text):
        items.append(messageOutputItem(
            id: newAnthropicID(prefix: "msg_"), text: text, status: "completed"))
        outputText = text
    case .toolCalls(let text, let calls):
        let prefix = textBeforeToolMarker(text)
        if !prefix.isEmpty {
            items.append(messageOutputItem(
                id: newAnthropicID(prefix: "msg_"), text: prefix, status: "completed"))
            outputText = prefix
        }
        for call in calls {
            items.append(functionCallOutputItem(
                id: newAnthropicID(prefix: "fc_"), callID: call.id, name: call.name,
                argumentsJSON: call.argumentsJSON, status: "completed"))
        }
    }
    return jsonResponse(completedResponsePayload(
        id: responseID, model: responsesRequest.model, request: responsesRequest,
        items: items, outputText: outputText,
        promptTokens: result.promptTokens, completionTokens: result.completionTokens,
        hitMaxTokens: result.hitMaxTokens))
}

/// Responses SSE event sequence for one response (standard runtime).
func responsesStream(
    emit: @escaping @Sendable (String) -> Void,
    runner: ModelRunner,
    messages: [ChatMessage],
    toolsJSON: Data?,
    maxNewTokens: Int,
    temperature: Double,
    topK: Int,
    responseID: String,
    model: String,
    request: ResponsesRequest
) async throws {
    let liveText = toolsJSON == nil
    let messageID = newAnthropicID(prefix: "msg_")

    emit(ResponsesSSE.responseState("response.created", id: responseID, model: model))
    emit(ResponsesSSE.responseState("response.in_progress", id: responseID, model: model))

    // onPromptTokens fires after prompt tokenization but before any generation
    // delta, so event ordering (output_item.added → content_part.added →
    // deltas) is preserved.
    let onPromptTokens: @Sendable (Int) -> Void = { _ in
        if liveText {
            emit(ResponsesSSE.outputItemAdded(
                outputIndex: 0,
                item: messageOutputItem(id: messageID, text: nil, status: "in_progress")))
            emit(ResponsesSSE.contentPartAdded(itemID: messageID, outputIndex: 0, contentIndex: 0))
        }
    }
    var onText: (@Sendable (String) -> Void)?
    if liveText {
        onText = { delta in
            emit(ResponsesSSE.outputTextDelta(
                itemID: messageID, outputIndex: 0, contentIndex: 0, delta: delta))
        }
    }

    let result = try await runChatRound(
        runner: runner,
        messages: messages,
        toolsJSON: toolsJSON,
        maxNewTokens: maxNewTokens,
        temperature: temperature,
        topK: topK,
        idPrefix: "call_",
        onText: onText,
        onPromptTokens: onPromptTokens
    )

    switch result.outcome {
    case .text(let text):
        if !liveText {
            // Tools were declared but the model answered in plain text.
            emitBufferedResponsesOutcome(
                emit: emit, responseID: responseID, model: model, request: request,
                text: text, calls: [],
                promptTokens: result.promptTokens, completionTokens: result.completionTokens,
                hitMaxTokens: result.hitMaxTokens)
            return
        }
        emit(ResponsesSSE.outputTextDone(
            itemID: messageID, outputIndex: 0, contentIndex: 0, text: text))
        emit(ResponsesSSE.contentPartDone(
            itemID: messageID, outputIndex: 0, contentIndex: 0, text: text))
        let item = messageOutputItem(id: messageID, text: text, status: "completed")
        emit(ResponsesSSE.outputItemDone(outputIndex: 0, item: item))
        emit(ResponsesSSE.completed(response: completedResponsePayload(
            id: responseID, model: model, request: request,
            items: [item], outputText: text,
            promptTokens: result.promptTokens, completionTokens: result.completionTokens,
            hitMaxTokens: result.hitMaxTokens)))
    case .toolCalls(let text, let calls):
        let prefix = textBeforeToolMarker(text)
        emitBufferedResponsesOutcome(
            emit: emit, responseID: responseID, model: model, request: request,
            text: prefix.isEmpty ? nil : prefix, calls: calls,
            promptTokens: result.promptTokens, completionTokens: result.completionTokens,
            hitMaxTokens: result.hitMaxTokens)
    }
}

// MARK: - External (coreml-llm) backend

func handleExternalResponse(
    _ responsesRequest: ResponsesRequest,
    context: ServerContext,
    messages: [ChatMessage],
    toolsJSON: Data?,
    maxNewTokens: Int
) async throws -> Response {
    guard let clm = context.coremlLLM else { return coremlLLMUnavailable() }
    let responseID = newAnthropicID(prefix: "resp_")
    let model = responsesRequest.model

    if responsesRequest.stream == true {
        let request = responsesRequest
        return streamingResponse { emit in
            emit(ResponsesSSE.responseState("response.created", id: responseID, model: model))
            emit(ResponsesSSE.responseState("response.in_progress", id: responseID, model: model))
            let (text, calls) = try await runExternalResponseOnce(
                clm: clm, modelName: model, messages: messages,
                toolsJSON: toolsJSON, maxNewTokens: maxNewTokens)
            emitBufferedResponsesOutcome(
                emit: emit, responseID: responseID, model: model, request: request,
                text: text.isEmpty ? nil : text, calls: calls,
                promptTokens: 0, completionTokens: 0, hitMaxTokens: false)
        }
    }

    let (text, calls) = try await runExternalResponseOnce(
        clm: clm, modelName: model, messages: messages,
        toolsJSON: toolsJSON, maxNewTokens: maxNewTokens)

    var items: [[String: Any]] = []
    var outputText = ""
    if !text.isEmpty {
        items.append(messageOutputItem(
            id: newAnthropicID(prefix: "msg_"), text: text, status: "completed"))
        outputText = text
    }
    for call in calls {
        items.append(functionCallOutputItem(
            id: newAnthropicID(prefix: "fc_"), callID: call.id, name: call.name,
            argumentsJSON: call.argumentsJSON, status: "completed"))
    }
    return jsonResponse(completedResponsePayload(
        id: responseID, model: model, request: responsesRequest,
        items: items, outputText: outputText,
        promptTokens: 0, completionTokens: 0, hitMaxTokens: false))
}

/// External backends expose no streaming/token accounting here — the full
/// generation is buffered, then surfaced as one outcome (text and/or calls).
func runExternalResponseOnce(
    clm: CoreMLLLMManager,
    modelName: String,
    messages: [ChatMessage],
    toolsJSON: Data?,
    maxNewTokens: Int
) async throws -> (String, [ChatToolCall]) {
    let model = try await clm.model(for: modelName)
    let raw = try await model.generate(
        messages: messages, toolsJSON: toolsJSON, maxTokens: maxNewTokens) { _ in }
    let (cleanText, parsed) = Gemma4Tools.parse(raw)
    let calls = parsed.map {
        ChatToolCall(id: "call_" + newToolCallID(), name: $0.name, argumentsJSON: $0.argumentsJSON)
    }
    return (calls.isEmpty ? cleanText : textBeforeToolMarker(cleanText), calls)
}
