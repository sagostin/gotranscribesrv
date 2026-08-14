import Foundation
import EmbeddingRuntime
import ExternalRuntime
import ImageRuntime
import ModelRuntime
import Tooling
import Vapor

struct ServerContext: Sendable {
    let settings: ServerSettings
    let manager: ModelManager
    let images: ImageModelManager?      // nil when the images feature is disabled
    let embeddings: EmbeddingModelManager? // nil when embeddings are disabled
    let coremlLLM: CoreMLLLMManager?  // nil if no coreml-llm entries
}

/// Outcome of one generation round, with token accounting.
struct ChatRoundResult: Sendable {
    enum Outcome: Sendable {
        case text(String)
        case toolCalls(String, [ChatToolCall])
    }
    var outcome: Outcome
    var promptTokens: Int
    var completionTokens: Int
    var hitMaxTokens: Bool
}

/// Render -> generate -> parse. Tool execution is the client's job: we only
/// surface the model's tool calls in the response.
/// onPromptTokens (optional) fires with the prompt token count right after
/// tokenization, before generation starts — lets SSE dialects emit their
/// opening frame with real input-token usage (e.g. Anthropic message_start).
func runChatRound(
    runner: ModelRunner,
    messages: [ChatMessage],
    toolsJSON: Data?,
    maxNewTokens: Int,
    temperature: Double,
    topK: Int,
    idPrefix: String = "call_",
    onText: (@Sendable (String) -> Void)? = nil,
    onPromptTokens: (@Sendable (Int) -> Void)? = nil
) async throws -> ChatRoundResult {
    let tokens = try await runner.applyChat(messages: messages, toolsJSON: toolsJSON)
    onPromptTokens?(tokens.count)
    // With tools in play the output must be parsed before we know what it is,
    // so it is buffered; plain chat streams deltas live.
    let output = try await runner.generate(
        tokens: tokens,
        maxNewTokens: maxNewTokens,
        temperature: temperature,
        topK: topK,
        onDelta: toolsJSON == nil ? onText : nil
    )
    let generated = Array(output.dropFirst(tokens.count))
    let text = await runner.decode(tokens: generated)
    let hitMax = generated.count >= maxNewTokens

    if toolsJSON != nil, let parsed = ToolCallParser.parse(text), !parsed.isEmpty {
        let calls = parsed.map {
            ChatToolCall(id: idPrefix + newToolCallID(), name: $0.name, argumentsJSON: $0.argumentsJSON)
        }
        return ChatRoundResult(
            outcome: .toolCalls(text, calls),
            promptTokens: tokens.count, completionTokens: generated.count, hitMaxTokens: hitMax)
    }
    if toolsJSON != nil { onText?(text) } // buffered final answer for tool-enabled requests
    return ChatRoundResult(
        outcome: .text(text),
        promptTokens: tokens.count, completionTokens: generated.count, hitMaxTokens: hitMax)
}

/// Mistral-safe id: 9 alphanumeric chars.
func newToolCallID() -> String {
    let alphabet = Array("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
    return String((0..<9).map { _ in alphabet.randomElement()! })
}

/// Anthropic-style id.
func newAnthropicID(prefix: String) -> String {
    let alphabet = Array("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
    return prefix + String((0..<24).map { _ in alphabet.randomElement()! })
}

func routes(_ app: Application, context: ServerContext) throws {
    app.get("health") { _ in
        ["status": "ok"]
    }

    // MARK: Model registry / lifecycle

    app.get("v1", "models") { req async throws -> Response in
        let entries = await context.manager.entries
        var data: [[String: Any]] = []
        for entry in entries {
            let status: ModelStatus
            switch entry.kind {
            case .chat:
                if entry.runtime == .coremlLLM {
                    status = await context.coremlLLM?.status(id: entry.id) ?? .notDownloaded
                } else {
                    status = await context.manager.status(id: entry.id)
                }
            case .image: status = await context.images?.status(id: entry.id) ?? .notDownloaded
            case .embedding: status = await context.embeddings?.status(id: entry.id) ?? .notDownloaded
            }
            data.append([
                "id": entry.id,
                "object": "model",
                "created": 0,
                "owned_by": "local",
                "kind": entry.kind.rawValue,
                "runtime": entry.runtime.rawValue,
                "repo": entry.repo,
                "status": status.label,
                "preload": entry.preload,
                "notes": entry.notes ?? "",
            ])
        }
        return jsonResponse(["object": "list", "data": data])
    }

    app.get("models", ":id", "status") { req async throws -> Response in
        guard let id = req.parameters.get("id") else { throw Abort(.badRequest) }
        guard let entry = await context.manager.entries.first(where: { $0.id == id }) else {
            return unknownModelResponse(id, entries: await context.manager.entries)
        }
        let status: ModelStatus
        switch entry.kind {
        case .chat:
            if entry.runtime == .coremlLLM {
                status = await context.coremlLLM?.status(id: id) ?? .notDownloaded
            } else {
                status = await context.manager.status(id: id)
            }
        case .image: status = await context.images?.status(id: id) ?? .notDownloaded
        case .embedding: status = await context.embeddings?.status(id: id) ?? .notDownloaded
        }
        return jsonResponse(["id": id, "status": status.label])
    }

    app.post("models", ":id", "download") { req async throws -> Response in
        guard let id = req.parameters.get("id") else { throw Abort(.badRequest) }
        guard let entry = await context.manager.entries.first(where: { $0.id == id }) else {
            return unknownModelResponse(id, entries: await context.manager.entries)
        }
        Task {
            do {
                switch (entry.kind, entry.runtime) {
                case (.chat, .coremlLLM): try await context.coremlLLM?.download(id: id)
                case (.chat, _): try await context.manager.download(id: id)
                case (.image, _): try await context.images?.download(id: id)
                case (.embedding, _): try await context.embeddings?.download(id: id)
                }
                print("[server] download finished: \(id)")
            } catch {
                print("[server] download failed for \(id): \(error.localizedDescription)")
            }
        }
        return jsonResponse(["id": id, "status": "download_started"], status: .accepted)
    }

    app.post("models", ":id", "load") { req async throws -> Response in
        guard let id = req.parameters.get("id") else { throw Abort(.badRequest) }
        guard let entry = await context.manager.entries.first(where: { $0.id == id }) else {
            return unknownModelResponse(id, entries: await context.manager.entries)
        }
        do {
            switch (entry.kind, entry.runtime) {
            case (.chat, .coremlLLM):
                guard let clm = context.coremlLLM else { return coremlLLMUnavailable() }
                try await clm.load(id: id)
            case (.chat, _): _ = try await context.manager.runner(for: id)
            case (.image, _):
                guard let images = context.images else { return imagesDisabled() }
                try await images.load(id: id)
            case (.embedding, _):
                guard let embeddings = context.embeddings else { return embeddingsDisabled() }
                try await embeddings.load(id: id)
            }
            return jsonResponse(["id": id, "status": "ready"])
        } catch let error as ModelError {
            return errorResponse(error.httpStatus, message: error.description, type: "invalid_request_error")
        } catch {
            return errorResponse(.internalServerError, message: error.localizedDescription, type: "server_error")
        }
    }

    app.post("models", ":id", "unload") { req async throws -> Response in
        guard let id = req.parameters.get("id") else { throw Abort(.badRequest) }
        guard let entry = await context.manager.entries.first(where: { $0.id == id }) else {
            return unknownModelResponse(id, entries: await context.manager.entries)
        }
        switch (entry.kind, entry.runtime) {
        case (.chat, .coremlLLM): await context.coremlLLM?.unload(id: id)
        case (.chat, _): await context.manager.unload(id: id)
        case (.image, _): await context.images?.unload(id: id)
        case (.embedding, _): await context.embeddings?.unload(id: id)
        }
        return jsonResponse(["id": id, "status": "unloaded"])
    }

    // MARK: OpenAI chat

    app.post("v1", "chat", "completions") { req async throws -> Response in
        let chatRequest = try req.content.decode(ChatCompletionRequest.self)
        guard let entry = await context.manager.entries.first(where: { $0.id == chatRequest.model }) else {
            return unknownModelResponse(chatRequest.model, entries: await context.manager.entries)
        }

        let entryMax = entry.maxNewTokens
        let maxNewTokens = chatRequest.maxTokens ?? entryMax ?? 512

        if entry.runtime == .coremlLLM {
            // External backend (gemma, etc.). Tool calling is supported via the
            // gemma-4 token format; see Gemma4Tools.
            guard let clm = context.coremlLLM else { return coremlLLMUnavailable() }
            do {
                return try await handleExternalChat(
                    chatRequest: chatRequest,
                    context: context,
                    maxNewTokens: maxNewTokens)
            } catch let error as ModelError {
                return errorResponse(error.httpStatus, message: error.description, type: "invalid_request_error")
            } catch {
                return errorResponse(.internalServerError, message: error.localizedDescription, type: "server_error")
            }
        }

        let runner: ModelRunner
        do {
            runner = try await context.manager.runner(for: chatRequest.model)
        } catch let error as ModelError {
            return errorResponse(error.httpStatus, message: error.description, type: "invalid_request_error")
        }
        let toolsJSON = chatRequest.toolChoiceIsNone ? nil : chatRequest.toolsJSON

        if chatRequest.stream == true {
            return streamingResponse { emit in
                let result = try await runChatRound(
                    runner: runner,
                    messages: chatRequest.chatMessages,
                    toolsJSON: toolsJSON,
                    maxNewTokens: maxNewTokens,
                    temperature: chatRequest.temperature ?? 0,
                    topK: chatRequest.topK ?? 50,
                    onText: { delta in emit(SSEChunk.delta(delta)) }
                )
                var finishReason = result.hitMaxTokens ? "length" : "stop"
                if case .toolCalls(_, let calls) = result.outcome {
                    for (index, call) in calls.enumerated() {
                        emit(SSEChunk.toolCall(
                            index: index, id: call.id, name: call.name,
                            argumentsJSON: call.argumentsJSON))
                    }
                    finishReason = "tool_calls"
                }
                emit(SSEChunk.finish(
                    finishReason,
                    usage: [
                        "prompt_tokens": result.promptTokens,
                        "completion_tokens": result.completionTokens,
                        "total_tokens": result.promptTokens + result.completionTokens,
                    ]))
                emit(SSEChunk.done)
            }
        }

        let result = try await runChatRound(
            runner: runner,
            messages: chatRequest.chatMessages,
            toolsJSON: toolsJSON,
            maxNewTokens: maxNewTokens,
            temperature: chatRequest.temperature ?? 0,
            topK: chatRequest.topK ?? 50
        )

        let message: [String: Any]
        var finishReason = result.hitMaxTokens ? "length" : "stop"
        switch result.outcome {
        case .text(let text):
            message = ["role": "assistant", "content": text]
        case .toolCalls(_, let calls):
            message = [
                "role": "assistant",
                "content": NSNull(),
                "tool_calls": calls.map { call in
                    [
                        "id": call.id,
                        "type": "function",
                        "function": ["name": call.name, "arguments": call.argumentsJSON],
                    ] as [String: Any]
                },
            ]
            finishReason = "tool_calls"
        }
        return jsonResponse([
            "id": "chatcmpl-\(UUID().uuidString)",
            "object": "chat.completion",
            "created": Int(Date().timeIntervalSince1970),
            "model": chatRequest.model,
            "choices": [[
                "index": 0,
                "finish_reason": finishReason,
                "message": message,
            ]],
            "usage": [
                "prompt_tokens": result.promptTokens,
                "completion_tokens": result.completionTokens,
                "total_tokens": result.promptTokens + result.completionTokens,
            ],
        ])
    }

    // MARK: OpenAI legacy completions

    app.post("v1", "completions") { req async throws -> Response in
        let completionRequest = try req.content.decode(CompletionRequest.self)
        let runner: ModelRunner
        do {
            runner = try await context.manager.runner(for: completionRequest.model)
        } catch let error as ModelError {
            return errorResponse(error.httpStatus, message: error.description, type: "invalid_request_error")
        }
        let entry = await context.manager.entries.first { $0.id == completionRequest.model }
        let maxNewTokens = completionRequest.maxTokens ?? entry?.maxNewTokens ?? 128

        let promptTokens = try await runner.encodeRaw(completionRequest.prompt)

        if completionRequest.stream == true {
            return streamingResponse { emit in
                let output = try await runner.generate(
                    tokens: promptTokens,
                    maxNewTokens: maxNewTokens,
                    temperature: completionRequest.temperature ?? 0,
                    topK: completionRequest.topK ?? 50,
                    onDelta: { delta in
                        emit(SSEChunk.raw([
                            "object": "text_completion",
                            "choices": [["index": 0, "text": delta, "finish_reason": NSNull()]],
                        ]))
                    })
                emit(SSEChunk.raw([
                    "object": "text_completion",
                    "choices": [[
                        "index": 0, "text": "",
                        "finish_reason": output.count - promptTokens.count >= maxNewTokens ? "length" : "stop",
                    ]],
                ]))
                emit(SSEChunk.done)
            }
        }

        let output = try await runner.generate(
            tokens: promptTokens,
            maxNewTokens: maxNewTokens,
            temperature: completionRequest.temperature ?? 0,
            topK: completionRequest.topK ?? 50
        )
        let generated = Array(output.dropFirst(promptTokens.count))
        let text = await runner.decode(tokens: generated)
        return jsonResponse([
            "id": "cmpl-\(UUID().uuidString)",
            "object": "text_completion",
            "created": Int(Date().timeIntervalSince1970),
            "model": completionRequest.model,
            "choices": [[
                "index": 0,
                "text": text,
                "finish_reason": generated.count >= maxNewTokens ? "length" : "stop",
            ]],
            "usage": [
                "prompt_tokens": promptTokens.count,
                "completion_tokens": generated.count,
                "total_tokens": output.count,
            ],
        ])
    }

    // MARK: OpenAI embeddings

    app.post("v1", "embeddings") { req async throws -> Response in
        guard let embeddings = context.embeddings else { return embeddingsDisabled() }
        let embeddingRequest = try req.content.decode(EmbeddingRequest.self)
        do {
            let result = try await embeddings.embed(
                id: embeddingRequest.model, inputs: embeddingRequest.inputs)
            let data = result.vectors.enumerated().map { index, vector in
                [
                    "object": "embedding",
                    "index": index,
                    "embedding": vector,
                ] as [String: Any]
            }
            return jsonResponse([
                "object": "list",
                "data": data,
                "model": embeddingRequest.model,
                "usage": [
                    "prompt_tokens": result.promptTokens,
                    "total_tokens": result.promptTokens,
                ],
            ])
        } catch let error as ModelError {
            return errorResponse(error.httpStatus, message: error.description, type: "invalid_request_error")
        }
    }

    // MARK: OpenAI images

    app.post("v1", "images", "generations") { req async throws -> Response in
        guard let images = context.images else { return imagesDisabled() }
        let imageRequest = try req.content.decode(ImageGenerationRequest.self)
        let entry = await context.manager.entries.first { $0.id == imageRequest.model }
        let nativeSize = entry?.imageSize ?? 512
        if let size = imageRequest.size, size != "\(nativeSize)x\(nativeSize)" {
            return errorResponse(
                .badRequest,
                message: "Model \(imageRequest.model) only supports \(nativeSize)x\(nativeSize)",
                type: "invalid_request_error")
        }
        do {
            let generated = try await images.generate(
                id: imageRequest.model,
                prompt: imageRequest.prompt,
                negativePrompt: imageRequest.negativePrompt ?? "",
                count: imageRequest.n ?? 1,
                steps: imageRequest.steps ?? 25,
                guidanceScale: Float(imageRequest.guidanceScale ?? 7.5),
                seed: imageRequest.seed
            )
            let data = generated.map { image in
                ["b64_json": image.pngData.base64EncodedString()]
            }
            return jsonResponse([
                "created": Int(Date().timeIntervalSince1970),
                "data": data,
            ])
        } catch let error as ModelError {
            return errorResponse(error.httpStatus, message: error.description, type: "invalid_request_error")
        } catch {
            return errorResponse(.internalServerError, message: error.localizedDescription, type: "server_error")
        }
    }

    // MARK: Anthropic Messages API

    anthropicRoutes(app, context: context)

    // MARK: OpenAI Responses API

    responsesRoutes(app, context: context)
}

// MARK: - Response helpers

/// Handler for chat completion requests routed to the CoreML-LLM external backend.
func handleExternalChat(
    chatRequest: ChatCompletionRequest,
    context: ServerContext,
    maxNewTokens: Int
) async throws -> Response {
    guard let clm = context.coremlLLM else {
        return coremlLLMUnavailable()
    }
    let model = try await clm.model(for: chatRequest.model)
    let messages = chatRequest.chatMessages
    let toolsJSON = chatRequest.toolChoiceIsNone ? nil : chatRequest.toolsJSON

    if chatRequest.stream == true {
        return streamingResponse { emit in
            let text = try await model.generate(
                messages: messages,
                toolsJSON: toolsJSON,
                maxTokens: maxNewTokens
            ) { delta in
                emit(SSEChunk.delta(delta))
            }
            // Gemma-4 may emit `<|tool_call>...<tool_call|>` blocks; surface them
            // as OpenAI-shape `tool_calls` chunks so downstream clients can
            // route them through their MCP.
            let (_, calls) = Gemma4Tools.parse(text)
            if !calls.isEmpty {
                for (index, call) in calls.enumerated() {
                    emit(SSEChunk.toolCall(
                        index: index,
                        id: "toolu_" + newToolCallID(),
                        name: call.name,
                        argumentsJSON: call.argumentsJSON))
                }
                emit(SSEChunk.finish("tool_calls"))
            } else {
                emit(SSEChunk.finish("stop"))
            }
            emit(SSEChunk.done)
        }
    }
    let text = try await model.generate(
        messages: messages, toolsJSON: toolsJSON, maxTokens: maxNewTokens) { _ in }
    let (_, calls) = Gemma4Tools.parse(text)
    let message: [String: Any]
    let finishReason: String
    if calls.isEmpty {
        message = ["role": "assistant", "content": text]
        finishReason = "stop"
    } else {
        let toolBlocks: [[String: Any]] = calls.enumerated().map { _, call in
            [
                "id": "toolu_" + newToolCallID(),
                "type": "function",
                "function": ["name": call.name, "arguments": call.argumentsJSON],
            ] as [String: Any]
        }
        message = ["role": "assistant", "content": NSNull(), "tool_calls": toolBlocks]
        finishReason = "tool_calls"
    }
    return jsonResponse([
        "id": "chatcmpl-\(UUID().uuidString)",
        "object": "chat.completion",
        "created": Int(Date().timeIntervalSince1970),
        "model": chatRequest.model,
        "choices": [[
            "index": 0,
            "finish_reason": finishReason,
            "message": message,
        ]],
        "usage": ["prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0],
    ])
}

func jsonResponse(_ object: [String: Any], status: HTTPResponseStatus = .ok) -> Response {
    let data = (try? JSONSerialization.data(withJSONObject: object)) ?? Data("{}".utf8)
    let response = Response(status: status, body: .init(data: data))
    response.headers.contentType = .json
    return response
}

/// OpenAI-style error envelope. `code` is machine-readable (e.g.
/// "model_not_found") so gateways and SDKs can branch on it — and so the
/// Go gateway's request logs show a non-empty error_code.
func errorResponse(_ status: HTTPResponseStatus, message: String, type: String, code: String? = nil) -> Response {
    let codeValue: Any = code ?? NSNull()
    return jsonResponse(
        ["error": ["message": message, "type": type, "code": codeValue]],
        status: status)
}

/// 404 for an unregistered model id. The message lists the available ids so
/// misconfigured clients (e.g. an SDK defaulting to "gpt-4o-mini") can see
/// exactly what this server serves, straight from the error body.
func unknownModelResponse(_ id: String, entries: [ModelRegistryEntry]) -> Response {
    let available = entries.map(\.id).sorted()
    let list = available.isEmpty ? "none configured" : available.joined(separator: ", ")
    return errorResponse(
        .notFound,
        message: "Unknown model: \(id). Available models: \(list)",
        type: "invalid_request_error",
        code: "model_not_found")
}

func imagesDisabled() -> Response {
    errorResponse(.forbidden, message: "Image generation is disabled on this server", type: "feature_disabled")
}

func embeddingsDisabled() -> Response {
    errorResponse(.forbidden, message: "Embeddings are disabled on this server", type: "feature_disabled")
}

func coremlLLMUnavailable() -> Response {
    errorResponse(
        .notFound,
        message: "External runtime not available (no CoreML-LLM entries configured)",
        type: "invalid_request_error")
}

extension ModelError {
    var httpStatus: HTTPResponseStatus {
        switch self {
        case .unknownModel: return .notFound
        case .autoDownloadDisabled: return .conflict
        case .wrongKind, .incompatibleLayout, .packageNotFound: return .badRequest
        default: return .internalServerError
        }
    }
}

/// SSE response; `emit` sends one raw SSE frame.
func streamingResponse(
    _ handler: @escaping @Sendable (@escaping @Sendable (String) -> Void) async throws -> Void
) -> Response {
    let response = Response(status: .ok)
    response.headers.contentType = HTTPMediaType(type: "text", subType: "event-stream")
    response.headers.cacheControl = .init(noCache: true)
    response.body = .init(stream: { writer in
        Task {
            do {
                try await handler { chunk in
                    _ = writer.write(.buffer(ByteBuffer(string: chunk)))
                }
            } catch {
                let reason = error.localizedDescription.replacingOccurrences(of: "\"", with: "'")
                _ = writer.write(.buffer(ByteBuffer(string: "data: {\"error\": \"\(reason)\"}\n\n")))
            }
            _ = writer.write(.end)
        }
    })
    return response
}
