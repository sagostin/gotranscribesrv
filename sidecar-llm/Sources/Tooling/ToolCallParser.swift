import Foundation

public struct ParsedToolCall: Sendable, Equatable {
    public var name: String
    /// Arguments serialized as a JSON object string.
    public var argumentsJSON: String
    /// Model-generated call id (Mistral emits 9-char alphanumerics), if present.
    public var id: String?
}

/// Extracts tool calls from generated text. Expects the Mistral-style
/// `[TOOL_CALLS] [{"name": ..., "arguments": {...}}]` marker; tolerant of the
/// marker being missing when the output is exactly a JSON array of calls.
public enum ToolCallParser {
    public static func parse(_ text: String) -> [ParsedToolCall]? {
        let candidate: String
        if let markerRange = text.range(of: "[TOOL_CALLS]") {
            candidate = String(text[markerRange.upperBound...])
        } else {
            let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
            guard trimmed.hasPrefix("[") else { return nil }
            candidate = trimmed
        }
        guard let arrayString = extractJSONArray(from: candidate),
              let data = arrayString.data(using: .utf8),
              let raw = try? JSONSerialization.jsonObject(with: data) as? [[String: Any]]
        else {
            return nil
        }
        let calls: [ParsedToolCall] = raw.compactMap { item in
            guard let name = item["name"] as? String else { return nil }
            let argumentsJSON: String
            if let args = item["arguments"] as? [String: Any],
               let argsData = try? JSONSerialization.data(withJSONObject: args),
               let argsString = String(data: argsData, encoding: .utf8)
            {
                argumentsJSON = argsString
            } else if let argsString = item["arguments"] as? String {
                argumentsJSON = argsString
            } else {
                argumentsJSON = "{}"
            }
            return ParsedToolCall(name: name, argumentsJSON: argumentsJSON, id: item["id"] as? String)
        }
        return calls.isEmpty ? nil : calls
    }

    /// Balanced-bracket scan for the first top-level JSON array, string-aware.
    static func extractJSONArray(from text: String) -> String? {
        guard let start = text.firstIndex(of: "[") else { return nil }
        var depth = 0
        var inString = false
        var escaped = false
        var index = start
        while index < text.endIndex {
            let char = text[index]
            if inString {
                if escaped {
                    escaped = false
                } else if char == "\\" {
                    escaped = true
                } else if char == "\"" {
                    inString = false
                }
            } else if char == "\"" {
                inString = true
            } else if char == "[" {
                depth += 1
            } else if char == "]" {
                depth -= 1
                if depth == 0 {
                    return String(text[start...index])
                }
            }
            index = text.index(after: index)
        }
        return nil
    }
}
