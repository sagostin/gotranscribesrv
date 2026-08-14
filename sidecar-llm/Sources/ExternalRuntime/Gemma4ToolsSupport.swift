import CoreMLLLM
import Foundation

/// Formatting and parsing helpers for the gemma-4 function-calling format.
///
/// The official gemma-4 it chat template renders tool definitions inside a
/// `<|turn>system\n<|tool>decl...<tool|>...<turn|>` block. We can't ask
/// CoreML-LLM's buildGemmaPrompt to emit that block (it doesn't expose
/// `tools`), so we inject the same `<|tool>...<tool|>` declarations directly
/// into the FIRST user-turn content. The model was trained to recognize tool
/// defs wherever they appear in the conversation context.
///
/// Tool responses are rendered as `<|tool_response>response:NAME{value:...}<tool_response|>`.
/// We synthesize these from consecutive `role:"tool"` ChatMessages and inject
/// them into the next user-turn content (or as a synthetic user message when
/// no further user turn follows).
///
/// Function-call format:
///   `<|tool_call>call:NAME{arg:val,arg:val}<tool_call|>`
public enum Gemma4Tools {
    public struct ParsedCall {
        public var name: String
        public var argumentsJSON: String // JSON object string
    }

    // MARK: - Format

    /// Render a JSON tool array (OpenAI/our toolsJSON shape) as the gemma-4
    /// tool-declaration block. Returns the empty string if `toolsJSON` is nil
    /// or yields no tools.
    public static func renderToolDeclarations(toolsJSON: Data?) -> String {
        guard let toolsJSON,
              let root = try? JSONSerialization.jsonObject(with: toolsJSON),
              let tools = root as? [Any]
        else { return "" }

        var out = ""
        for tool in tools {
            guard let dict = tool as? [String: Any] else { continue }
            let function = (dict["function"] as? [String: Any]) ?? dict
            out += "<|tool>"
            out += renderDeclaration(function: function)
            out += "<tool|>\n"
        }
        return out
    }

    private static func renderDeclaration(function: [String: Any]) -> String {
        let q = quoteToken()
        let qOpen = "{" + "description:" + q
        var s = "declaration:"
        if let name = function["name"] as? String { s += name }
        if let desc = function["description"] as? String, !desc.isEmpty {
            s += qOpen + desc + q
        }
        if let params = function["parameters"] as? [String: Any] {
            s += ",parameters:{"
            if let properties = params["properties"] as? [String: Any] {
                s += "properties:{"
                s += formatParameters(properties: properties, required: params["required"] as? [String] ?? [])
                s += "}"
            }
            if let required = params["required"] as? [String], !required.isEmpty {
                let items = required.map { q + $0 + q }.joined(separator: ",")
                s += ",required:[" + items + "]"
            }
            if let type = params["type"] as? String {
                s += ",type:" + q + type.uppercased() + q
            }
            s += "}"
        }
        s += "}"
        return s
    }

    private static func formatParameters(properties: [String: Any], required: [String]) -> String {
        let q = quoteToken()
        let sorted = properties.sorted { $0.key < $1.key }
        var parts: [String] = []
        for (key, raw) in sorted {
            guard let value = raw as? [String: Any] else { continue }
            var piece = key
            piece += ":{"
            var first = true
            if let desc = value["description"] as? String {
                piece += "description:" + q + desc + q
                first = false
            }
            piece += first ? "" : ","
            piece += "type:" + q + ((value["type"] as? String ?? "string").uppercased()) + q
            piece += "}"
            parts.append(piece)
        }
        return parts.joined(separator: ",")
    }

    /// The gemma chat template's quoted-token literal — built from individual
    /// characters so Swift's parser doesn't snip on `<|...|>` substrings.
    private static func quoteToken() -> String {
        return "<" + "|" + "\"" + "|" + ">"
    }

    // MARK: - Pre-round prompt augmentation

    /// Inject tool declarations into the FIRST user-turn content. Returns the
    /// augmented message array and tool name registry (id -> name).
    public static func injectToolDefBlock(
        messages: [CoreMLLLM.Message],
        toolsJSON: Data?
    ) -> [CoreMLLLM.Message] {
        let block = renderToolDeclarations(toolsJSON: toolsJSON)
        guard !block.isEmpty else { return messages }
        // Find the FIRST user message and prepend.
        guard let idx = messages.firstIndex(where: { $0.role == .user }) else {
            // No user message at all — synthesize one carrying the tool block.
            return messages + [
                CoreMLLLM.Message(role: .system, content: block)
            ]
        }
        var copy = messages
        let existing = copy[idx].content
        copy[idx] = CoreMLLLM.Message(role: .user, content: block + "\n\n" + existing)
        return copy
    }

    /// Inject gemma-formatted tool responses into the next user message after
    /// the most recent assistant turn (or a synthetic user turn if none).
    /// `responses` is parallel to `messages.toolCallID`-bearing ChatMessages:
    /// each tuple is the function name and the response payload string.
    public static func injectToolResponseBlock(
        messages: [CoreMLLLM.Message],
        nameByCallID: [String: String],
        responsesByCallID: [String: String]
    ) -> [CoreMLLLM.Message] {
        guard !responsesByCallID.isEmpty else { return messages }

        var block = ""
        for (callID, value) in responsesByCallID {
            let name = nameByCallID[callID] ?? "function"
            block += "<|tool_response>"
            // Coerce the value into a JSON value: if it parses as JSON, use it raw;
            // otherwise JSON-string-encode it.
            let rendered: String
            if let data = value.data(using: .utf8),
               (try? JSONSerialization.jsonObject(with: data)) != nil
            {
                rendered = value
            } else {
                rendered = (try? String(data: JSONEncoder().encode(value), encoding: .utf8)) ?? "\"\""
            }
            block += "response:\(name){value:\(rendered)}"
            block += "<tool_response|>\n"
        }

        // Append a synthetic user message that delivers the responses.
        return messages + [CoreMLLLM.Message(role: .user, content: block)]
    }

    // MARK: - Parse

    /// Extract gemma-format tool calls from the model output. Returns the
    /// remaining plain text and the parsed calls.
    public static func parse(_ text: String) -> (text: String, calls: [ParsedCall]) {
        var calls: [ParsedCall] = []
        var remaining = text
        while let range = remaining.range(of: "<|tool_call>") {
            let after = remaining[range.upperBound...]
            guard let endRange = after.range(of: "<tool_call|>") else { break }
            let inner = String(after[..<endRange.lowerBound])
            // Drop the prefix before "<|tool_call>" from `remaining` as we go.
            remaining = String(remaining[range.upperBound...])
            // `inner` should look like "call:NAME{KEY:VAL,...}".
            guard let parsed = parseCallBody(inner) else { continue }
            calls.append(parsed)
            // Cut from `remaining` past "<tool_call|>".
            let endIndex = remaining.range(of: "<tool_call|>")?.upperBound
            if let endIndex {
                remaining = String(remaining[endIndex...])
            }
        }
        return (text: remaining, calls: calls)
    }

    /// Parse `call:NAME{arg:val,arg:val}` into a `ParsedCall`. Tolerant of
    /// extra whitespace.
    private static func parseCallBody(_ body: String) -> ParsedCall? {
        // Strip leading whitespace.
        let trimmed = String(body.drop(while: { $0 == " " || $0 == "\n" }))
        let prefix = "call:"
        guard trimmed.hasPrefix(prefix) else { return nil }
        let prefixEnd = trimmed.index(trimmed.startIndex, offsetBy: prefix.count)
        guard let braceIdx = trimmed[prefixEnd...].firstIndex(of: "{") else { return nil }
        let name = trimmed[prefixEnd..<braceIdx].trimmingCharacters(in: .whitespacesAndNewlines)
        guard let endBrace = matchBalanced(trimmed, from: braceIdx) else { return nil }
        let insideStart = trimmed.index(after: braceIdx)
        let argumentsLiteral = trimmed[insideStart..<endBrace]
        // Gemma's chat template represents JSON strings inside tool-call
        // arguments as `<|"|>...<|"|>`. Replace these with regular JSON quotes.
        let q = quoteToken()
        let argumentsJSON = "{" + argumentsLiteral.replacingOccurrences(of: q, with: "\"") + "}"
        return ParsedCall(name: name, argumentsJSON: argumentsJSON)
    }

    private static func matchBalanced(_ s: String, from openIdx: String.Index) -> String.Index? {
        var depth = 0
        var idx = openIdx
        var inString = false
        var escaped = false
        while idx < s.endIndex {
            let c = s[idx]
            if inString {
                if escaped { escaped = false }
                else if c == "\\" { escaped = true }
                else if c == "\"" { inString = false }
            } else if c == "\"" {
                inString = true
            } else if c == "{" {
                depth += 1
            } else if c == "}" {
                depth -= 1
                if depth == 0 { return idx }
            }
            idx = s.index(after: idx)
        }
        return nil
    }
}
