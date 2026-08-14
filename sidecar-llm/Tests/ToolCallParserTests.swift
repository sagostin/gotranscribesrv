import XCTest
@testable import Tooling

final class ToolCallParserTests: XCTestCase {
    func testParsesMistralToolCallMarker() {
        let text = #"[TOOL_CALLS] [{"name": "calculator", "arguments": {"expression": "2+2"}}]"#
        let calls = ToolCallParser.parse(text)
        XCTAssertEqual(calls?.count, 1)
        XCTAssertEqual(calls?.first?.name, "calculator")
        XCTAssertEqual(calls?.first?.argumentsJSON, #"{"expression":"2+2"}"#)
    }

    func testParsesMultipleCalls() {
        let text = #"[TOOL_CALLS] [{"name": "a", "arguments": {}}, {"name": "b", "arguments": {"x": 1}}]"#
        let calls = ToolCallParser.parse(text)
        XCTAssertEqual(calls?.map(\.name), ["a", "b"])
    }

    func testNoMarkerPlainTextReturnsNil() {
        XCTAssertNil(ToolCallParser.parse("The weather in Paris is 20 degrees."))
    }

    func testBareJSONArrayWithoutMarker() {
        let text = #" [{"name": "current_time", "arguments": {}}] "#
        let calls = ToolCallParser.parse(text)
        XCTAssertEqual(calls?.first?.name, "current_time")
    }

    func testArgumentsAsString() {
        let text = #"[TOOL_CALLS] [{"name": "a", "arguments": "{\"x\":1}"}]"#
        let calls = ToolCallParser.parse(text)
        XCTAssertEqual(calls?.first?.argumentsJSON, #"{"x":1}"#)
    }

    func testBracketBalancingWithBracketsInStrings() {
        let text = #"[TOOL_CALLS] [{"name": "a", "arguments": {"note": "see [1]"}}] and trailing text"#
        let calls = ToolCallParser.parse(text)
        XCTAssertEqual(calls?.count, 1)
    }
}
