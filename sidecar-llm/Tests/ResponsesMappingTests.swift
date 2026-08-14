import XCTest
@testable import Server
import ModelRuntime

final class ResponsesMappingTests: XCTestCase {
    private func json(_ string: String) throws -> JSONValue {
        try JSONDecoder().decode(JSONValue.self, from: Data(string.utf8))
    }

    func testStringInputBecomesUserMessage() throws {
        let messages = try ResponsesMapper.toChatMessages(
            instructions: nil, input: .string("hello"))
        XCTAssertEqual(messages, [ChatMessage(role: "user", content: "hello")])
    }

    func testInstructionsBecomeSystemMessage() throws {
        let messages = try ResponsesMapper.toChatMessages(
            instructions: "be terse", input: .string("hi"))
        XCTAssertEqual(messages.count, 2)
        XCTAssertEqual(messages[0], ChatMessage(role: "system", content: "be terse"))
        XCTAssertEqual(messages[1], ChatMessage(role: "user", content: "hi"))
    }

    func testMessageItemsWithTextParts() throws {
        let input = try json(#"""
        [
            {"type": "message", "role": "developer",
             "content": [{"type": "input_text", "text": "sys rules"}]},
            {"type": "message", "role": "user",
             "content": [{"type": "input_text", "text": "part one"},
                          {"type": "input_text", "text": "part two"}]},
            {"role": "assistant",
             "content": [{"type": "output_text", "text": "earlier reply"}]}
        ]
        """#)
        let messages = try ResponsesMapper.toChatMessages(instructions: nil, input: input)
        XCTAssertEqual(messages, [
            ChatMessage(role: "system", content: "sys rules"),
            ChatMessage(role: "user", content: "part one\npart two"),
            ChatMessage(role: "assistant", content: "earlier reply"),
        ])
    }

    func testFunctionCallRoundTrip() throws {
        let input = try json(#"""
        [
            {"type": "function_call", "call_id": "call_abc", "name": "get_weather",
             "arguments": "{\"city\":\"Paris\"}"},
            {"type": "function_call_output", "call_id": "call_abc",
             "output": "{\"temp\": 20}"}
        ]
        """#)
        let messages = try ResponsesMapper.toChatMessages(instructions: nil, input: input)
        XCTAssertEqual(messages.count, 2)
        XCTAssertEqual(messages[0].role, "assistant")
        XCTAssertEqual(messages[0].toolCalls, [
            ChatToolCall(id: "call_abc", name: "get_weather", argumentsJSON: #"{"city":"Paris"}"#),
        ])
        XCTAssertEqual(messages[1], ChatMessage(role: "tool", content: #"{"temp": 20}"#, toolCallID: "call_abc"))
    }

    func testImageInputThrows() throws {
        let input = try json(#"""
        [{"type": "message", "role": "user",
          "content": [{"type": "input_image", "image_url": "https://example.com/x.png"}]}]
        """#)
        XCTAssertThrowsError(try ResponsesMapper.toChatMessages(instructions: nil, input: input)) { error in
            guard case ResponsesMapper.MapperError.unsupportedContent = error else {
                return XCTFail("expected unsupportedContent, got \(error)")
            }
        }
    }

    func testFunctionCallOutputWithContentParts() throws {
        let input = try json(#"""
        [{"type": "function_call_output", "call_id": "call_x",
          "output": [{"type": "input_text", "text": "tool said hi"}]}]
        """#)
        let messages = try ResponsesMapper.toChatMessages(instructions: nil, input: input)
        XCTAssertEqual(messages, [ChatMessage(role: "tool", content: "tool said hi", toolCallID: "call_x")])
    }

    func testUnknownItemsAreSkipped() throws {
        let input = try json(#"""
        [{"type": "reasoning", "summary": []},
         {"type": "message", "role": "user", "content": "hi"}]
        """#)
        let messages = try ResponsesMapper.toChatMessages(instructions: nil, input: input)
        XCTAssertEqual(messages, [ChatMessage(role: "user", content: "hi")])
    }

    private func decodeRequest(_ body: String) throws -> ResponsesRequest {
        struct Wrapper: Decodable { let value: ResponsesRequest }
        _ = Wrapper.self
        return try JSONDecoder().decode(ResponsesRequest.self, from: Data(body.utf8))
    }

    func testToolsJSONFlatToNestedConversion() throws {
        let request = try decodeRequest(#"""
        {"model": "m", "input": "hi",
         "tools": [
             {"type": "function", "name": "get_weather", "description": "w",
              "parameters": {"type": "object", "properties": {}}},
             {"type": "web_search"}
         ]}
        """#)
        let data = try XCTUnwrap(request.toolsJSON)
        let array = try XCTUnwrap(
            JSONSerialization.jsonObject(with: data) as? [[String: Any]])
        // Built-in tools are skipped; only the function survives.
        XCTAssertEqual(array.count, 1)
        XCTAssertEqual(array[0]["type"] as? String, "function")
        let function = try XCTUnwrap(array[0]["function"] as? [String: Any])
        XCTAssertEqual(function["name"] as? String, "get_weather")
    }

    func testToolChoiceNoneDetection() throws {
        let request = try decodeRequest(#"""
        {"model": "m", "input": "hi", "tool_choice": "none"}
        """#)
        XCTAssertTrue(request.toolChoiceIsNone)
    }
}
