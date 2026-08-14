import CoreMLLLM
import Foundation
import ModelRuntime

/// Wraps a CoreMLLLM model (e.g. mlboydaisuke/gemma-4-E2B-coreml) as a chat
/// backend for our OpenAI / Anthropic endpoints. CoreMLLLM owns its own
/// download, prompt templating, and inference engine.
///
/// Note: CoreMLLLM has its own chat templating and tool-calling format that
/// differs from Mistral's `[AVAILABLE_TOOLS]` / `[TOOL_CALLS]`. This backend
/// is text-only for v1; tool calls are not routed through it.
public final class CoreMLLLMModel: @unchecked Sendable {
    private let llm: CoreMLLLM
    public let entry: ModelRegistryEntry

    public init(entry: ModelRegistryEntry) async throws {
        self.entry = entry
        self.llm = try await CoreMLLLM.load(repo: entry.repo)
    }

    /// Generate text with optional tool definitions (gemma-4 format) and
    /// rounds that already contain tool calls/results (replayed via our
    /// ChatMessage type; only assistant text and role:"tool" messages are
    /// handed to CoreML-LLM).
    public func generate(
        messages: [ChatMessage],
        toolsJSON: Data?,
        maxTokens: Int,
        onDelta: @Sendable (String) -> Void
    ) async throws -> String {
        let (cmMessages, nameByCallID) = Self.materialize(messages: messages, toolsJSON: toolsJSON)
        var full = ""
        let tokenStream = try await llm.stream(cmMessages, maxTokens: maxTokens)
        for await token in tokenStream {
            full += token
            onDelta(token)
        }
        return full
    }

    private static func materialize(
        messages: [ChatMessage],
        toolsJSON: Data?
    ) -> ([CoreMLLLM.Message], [String: String]) {
        // Map nameByCallID for round-tripping tool calls across rounds.
        var nameByCallID: [String: String] = [:]
        for message in messages {
            guard let calls = message.toolCalls else { continue }
            for call in calls {
                nameByCallID[call.id] = call.name
            }
        }

        // Convert our ChatMessage into CoreML-LLM Messages. Tool messages get
        // batched by insertion after the assistant turn that produced the
        // matching call.
        var cmMessages: [CoreMLLLM.Message] = []
        var pendingResponses: [(callID: String, content: String)] = []

        func flushResponses() {
            guard !pendingResponses.isEmpty else { return }
            var dict: [String: String] = [:]
            for entry in pendingResponses { dict[entry.callID] = entry.content }
            let responsesByCallID = dict
            // Map already-built cmMessages (which are CoreMLLLM.Message) into
            // a synthetic extra user turn carrying the tool responses, then
            // append.
            let augmented = Gemma4Tools.injectToolResponseBlock(
                messages: cmMessages,
                nameByCallID: nameByCallID,
                responsesByCallID: responsesByCallID)
            for msg in augmented.dropFirst(cmMessages.count) {
                cmMessages.append(msg)
            }
            pendingResponses.removeAll()
        }

        for message in messages {
            switch message.role {
            case "system":
                cmMessages.append(CoreMLLLM.Message(role: .system, content: message.content ?? ""))
            case "user":
                flushResponses()
                cmMessages.append(CoreMLLLM.Message(role: .user, content: message.content ?? ""))
            case "assistant":
                flushResponses()
                if let calls = message.toolCalls, !calls.isEmpty {
                    // Render the assistant's previous call inline; CoreML-LLM
                    // doesn't support assistant tool_calls, so encode them in a
                    // model-friendly string for round-tripping.
                    let summary = calls.map { call in
                        "<tool_call>\(call.id):\(call.name){\(call.argumentsJSON)}</tool_call>"
                    }.joined(separator: "\n")
                    cmMessages.append(CoreMLLLM.Message(role: .assistant, content: summary))
                } else {
                    cmMessages.append(CoreMLLLM.Message(role: .assistant, content: message.content ?? ""))
                }
            case "tool":
                if let id = message.toolCallID {
                    pendingResponses.append((callID: id, content: message.content ?? ""))
                } else {
                    flushResponses()
                    cmMessages.append(CoreMLLLM.Message(role: .user, content: message.content ?? ""))
                }
            default:
                flushResponses()
                cmMessages.append(CoreMLLLM.Message(role: .user, content: message.content ?? ""))
            }
        }
        flushResponses()

        // Inject tool definitions into the FIRST user turn.
        let withToolDefs = Gemma4Tools.injectToolDefBlock(
            messages: cmMessages, toolsJSON: toolsJSON)
        return (withToolDefs, nameByCallID)
    }

    private static func translateRole(_ role: String) -> CoreMLLLM.Message.Role {
        switch role {
        case "user": return .user
        case "assistant": return .assistant
        case "system": return .system
        default: return .user
        }
    }
}

/// Per-model residency + status tracking, mirroring ModelManager's lifecycle.
public actor CoreMLLLMManager {
    private let settings: ServerSettings
    private var models: [String: CoreMLLLMModel] = [:]
    private var statuses: [String: ModelStatus] = [:]

    public init(settings: ServerSettings) {
        self.settings = settings
    }

    public func status(id: String) -> ModelStatus { statuses[id] ?? .notDownloaded }

    /// CoreMLLLM loads on demand (it handles its own download + model
    /// compilation), so download and load are effectively the same action.
    public func download(id: String) async throws {
        try await load(id: id)
    }

    public func load(id: String) async throws {
        guard let entry = externalEntry(for: id) else {
            throw ModelError.unknownModel(id)
        }
        if models[id] != nil { return }
        statuses[id] = .loading
        do {
            let model = try await CoreMLLLMModel(entry: entry)
            models[id] = model
            statuses[id] = .ready
        } catch {
            statuses[id] = .failed(error.localizedDescription)
            throw error
        }
    }

    public func unload(id: String) {
        models[id] = nil
        if statuses[id] == .ready { statuses[id] = .notDownloaded }
    }

    public func model(for id: String) async throws -> CoreMLLLMModel {
        if let model = models[id] { return model }
        try await load(id: id)
        guard let model = models[id] else {
            throw ModelError.unknownModel(id)
        }
        return model
    }

    private func externalEntry(for id: String) -> ModelRegistryEntry? {
        guard let entry = _entries?.first(where: { $0.id == id }) else { return nil }
        return entry
    }

    // The registry of entries the manager is responsible for (coreml-llm runtime).
    private var _entries: [ModelRegistryEntry]?

    public func setEntries(_ entries: [ModelRegistryEntry]) {
        self._entries = entries
        for entry in entries { statuses[entry.id] = .notDownloaded }
    }
}