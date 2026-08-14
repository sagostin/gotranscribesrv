import Foundation

/// A tool call emitted by the model (or replayed by the client in history).
public struct ChatToolCall: Codable, Sendable, Equatable {
    /// Client-facing call id (OpenAI-style `call_...` allowed; normalized at render time).
    public var id: String
    public var name: String
    /// Arguments serialized as a JSON object string.
    public var argumentsJSON: String

    public init(id: String, name: String, argumentsJSON: String) {
        self.id = id
        self.name = name
        self.argumentsJSON = argumentsJSON
    }
}

/// A chat message, including tool-calling history in the OpenAI shape.
public struct ChatMessage: Sendable, Equatable {
    public var role: String
    public var content: String?
    /// Present on assistant messages that requested tool calls.
    public var toolCalls: [ChatToolCall]?
    /// Present on tool messages: id of the call this is a result for.
    public var toolCallID: String?

    public init(
        role: String, content: String? = nil,
        toolCalls: [ChatToolCall]? = nil, toolCallID: String? = nil
    ) {
        self.role = role
        self.content = content
        self.toolCalls = toolCalls
        self.toolCallID = toolCallID
    }
}

/// Mistral's chat template requires 9-char alphanumeric tool call ids, while
/// OpenAI-style clients use `call_...`. Normalize deterministically so the same
/// client id always maps to the same model-side id within a conversation.
public func normalizedToolCallID(_ id: String) -> String {
    let alphanumeric = id.filter { $0.isLetter || $0.isNumber }
    if alphanumeric.count >= 9 {
        return String(alphanumeric.suffix(9))
    }
    // Deterministic pad: repeat the id's own characters.
    if alphanumeric.isEmpty { return "aaaaaaaaa" }
    var padded = alphanumeric
    var index = 0
    while padded.count < 9 {
        padded.append(alphanumeric[alphanumeric.index(alphanumeric.startIndex, offsetBy: index % alphanumeric.count)])
        index += 1
    }
    return padded
}
