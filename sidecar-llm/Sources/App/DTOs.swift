import ModelRuntime
import Vapor

struct ToolCallDTO: Content {
    var id: String?
    var type: String?
    var function: FunctionCallDTO
}

struct FunctionCallDTO: Content {
    var name: String
    /// Arguments as a JSON string (OpenAI convention).
    var arguments: String?
}

struct ChatMessageDTO: Content {
    var role: String
    var content: String?
    var toolCalls: [ToolCallDTO]?
    var toolCallID: String?

    enum CodingKeys: String, CodingKey {
        case role, content
        case toolCalls = "tool_calls"
        case toolCallID = "tool_call_id"
    }

    var asChatMessage: ChatMessage {
        ChatMessage(
            role: role,
            content: content,
            toolCalls: toolCalls?.map {
                ChatToolCall(
                    id: $0.id ?? "",
                    name: $0.function.name,
                    argumentsJSON: $0.function.arguments ?? "{}"
                )
            },
            toolCallID: toolCallID
        )
    }
}

struct ToolSpec: Content {
    var type: String?
    var function: FunctionSpec
}

struct FunctionSpec: Content {
    var name: String
    var description: String?
    var parameters: JSONValue?
}

/// OpenAI-flavored chat completion request.
struct ChatCompletionRequest: Content {
    var model: String
    var messages: [ChatMessageDTO]
    var stream: Bool?
    var maxTokens: Int?
    var temperature: Double?
    var topK: Int?
    var tools: [ToolSpec]?
    var toolChoice: JSONValue?

    enum CodingKeys: String, CodingKey {
        case model, messages, stream, temperature, tools
        case maxTokens = "max_tokens"
        case topK = "top_k"
        case toolChoice = "tool_choice"
    }

    var toolChoiceIsNone: Bool {
        if case .string(let value) = toolChoice { return value == "none" }
        if case .object(let object) = toolChoice,
           case .string(let type)? = object["type"] { return type == "none" }
        return false
    }

    /// Tool schemas in the shape chat templates expect, as a JSON array.
    var toolsJSON: Data? {
        guard let tools, !tools.isEmpty else { return nil }
        let array: [[String: Any]] = tools.map { spec in
            [
                "type": spec.type ?? "function",
                "function": [
                    "name": spec.function.name,
                    "description": spec.function.description ?? "",
                    "parameters": spec.function.parameters?.anyValue ?? [String: Any](),
                ],
            ]
        }
        return try? JSONSerialization.data(withJSONObject: array)
    }

    var chatMessages: [ChatMessage] {
        messages.map { $0.asChatMessage }
    }
}

/// OpenAI legacy completion request.
struct CompletionRequest: Content {
    var model: String
    var prompt: String
    var stream: Bool?
    var maxTokens: Int?
    var temperature: Double?
    var topK: Int?

    enum CodingKeys: String, CodingKey {
        case model, prompt, stream, temperature
        case maxTokens = "max_tokens"
        case topK = "top_k"
    }
}

/// OpenAI embeddings request.
struct EmbeddingRequest: Content {
    var model: String
    var input: JSONValue

    var inputs: [String] {
        switch input {
        case .string(let value): return [value]
        case .array(let values): return values.compactMap { value in
            if case .string(let string) = value { return string }
            return nil
        }
        default: return []
        }
    }
}

/// OpenAI image generation request.
struct ImageGenerationRequest: Content {
    var model: String
    var prompt: String
    var negativePrompt: String?
    var n: Int?
    var size: String?
    var steps: Int?
    var guidanceScale: Double?
    var seed: UInt32?
    var responseFormat: String?

    enum CodingKeys: String, CodingKey {
        case model, prompt, n, size, steps, seed
        case negativePrompt = "negative_prompt"
        case guidanceScale = "guidance_scale"
        case responseFormat = "response_format"
    }
}

enum SSEChunk {
    static func raw(_ payload: [String: Any]) -> String {
        guard let data = try? JSONSerialization.data(withJSONObject: payload),
              let json = String(data: data, encoding: .utf8)
        else { return "" }
        return "data: \(json)\n\n"
    }

    static func delta(_ text: String) -> String {
        frame(["content": text])
    }

    static func toolCall(index: Int, id: String, name: String, argumentsJSON: String) -> String {
        // OpenAI streams arguments as a string; we have the full call, so one chunk.
        frame([
            "tool_calls": [[
                "index": index,
                "id": id,
                "type": "function",
                "function": ["name": name, "arguments": argumentsJSON],
            ]],
        ])
    }

    static func finish(_ reason: String, usage: [String: Any]? = nil) -> String {
        var payload: [String: Any] = [
            "object": "chat.completion.chunk",
            "choices": [["index": 0, "delta": [String: Any](), "finish_reason": reason]],
        ]
        if let usage { payload["usage"] = usage }
        return raw(payload)
    }

    static let done = "data: [DONE]\n\n"

    private static func frame(_ delta: [String: Any]) -> String {
        raw([
            "object": "chat.completion.chunk",
            "choices": [["index": 0, "delta": delta]],
        ])
    }
}
