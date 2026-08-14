import XCTest
@testable import ModelRuntime

final class PromptRenderingTests: XCTestCase {
    func testSimpleUserTurn() {
        let text = ModelRunner.mistralPromptText(
            messages: [ChatMessage(role: "user", content: "Hi")], tools: nil)
        XCTAssertEqual(text, "[INST] Hi[/INST]")
    }

    func testSystemMergedIntoLastUserMessage() {
        let text = ModelRunner.mistralPromptText(
            messages: [
                ChatMessage(role: "system", content: "Be terse."),
                ChatMessage(role: "user", content: "Hello"),
                ChatMessage(role: "assistant", content: "Hi!"),
                ChatMessage(role: "user", content: "Bye"),
            ], tools: nil)
        XCTAssertEqual(text, "[INST] Hello[/INST] Hi!</s>[INST] Be terse.\n\nBye[/INST]")
    }

    func testToolsInsertedBeforeLastInst() {
        let tools: [[String: Any]] = [[
            "type": "function",
            "function": ["name": "calculator", "description": "Do math", "parameters": ["type": "object"]],
        ]]
        let text = ModelRunner.mistralPromptText(
            messages: [ChatMessage(role: "user", content: "2+2?")], tools: tools)
        XCTAssertTrue(text.hasPrefix("[AVAILABLE_TOOLS] ["))
        XCTAssertTrue(text.contains(#""name": "calculator""#))
        XCTAssertTrue(text.hasSuffix("[/AVAILABLE_TOOLS][INST] 2+2?[/INST]"))
    }

    func testToolCallAndResultHistory() {
        let text = ModelRunner.mistralPromptText(
            messages: [
                ChatMessage(role: "user", content: "2+2?"),
                ChatMessage(
                    role: "assistant",
                    toolCalls: [ChatToolCall(id: "call_abc123XYZ", name: "calculator", argumentsJSON: #"{"expression":"2+2"}"#)]
                ),
                ChatMessage(role: "tool", content: "4", toolCallID: "call_abc123XYZ"),
                ChatMessage(role: "assistant", content: "The answer is 4."),
            ], tools: nil)
        XCTAssertEqual(
            text,
            #"[INST] 2+2?[/INST][TOOL_CALLS] [{"name": "calculator", "arguments": {"expression":"2+2"}, "id": "abc123XYZ"}]</s>[TOOL_RESULTS] {"content": 4, "call_id": "abc123XYZ"}[/TOOL_RESULTS] The answer is 4.</s>"#
        )
    }

    func testToolResultStringContentIsQuoted() {
        let text = ModelRunner.mistralPromptText(
            messages: [ChatMessage(role: "tool", content: "sunny, 20C", toolCallID: "x")],
            tools: nil)
        XCTAssertTrue(text.contains(#""content": "sunny, 20C""#))
    }

    func testToolCallIDNormalization() {
        XCTAssertEqual(normalizedToolCallID("call_abc123XYZ"), "abc123XYZ")
        XCTAssertEqual(normalizedToolCallID("short"), "shortshor")
        XCTAssertEqual(normalizedToolCallID(""), "aaaaaaaaa")
        XCTAssertEqual(normalizedToolCallID("call_verylongidstring123456"), "ing123456")
    }
}
