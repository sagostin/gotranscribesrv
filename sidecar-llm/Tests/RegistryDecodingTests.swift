import XCTest
@testable import ModelRuntime

final class RegistryDecodingTests: XCTestCase {
    func testEntriesWithMissingDefaultedKeys() throws {
        let json = """
        {
          "settings": { "autoDownload": false },
          "models": [
            { "id": "a", "repo": "apple/mistral-coreml", "include": ["x/*"] },
            { "id": "img", "kind": "image", "repo": "apple/sd", "include": ["original/compiled/*"], "imageSize": 512 }
          ]
        }
        """
        let registry = try JSONDecoder().decode(ModelRegistry.self, from: Data(json.utf8))
        XCTAssertEqual(registry.settings.autoDownload, false)
        XCTAssertEqual(registry.settings.preload, true) // default
        XCTAssertEqual(registry.models.count, 2)
        XCTAssertEqual(registry.models[0].kind, .chat) // default
        XCTAssertEqual(registry.models[0].maxNewTokens, 512) // default
        XCTAssertEqual(registry.models[1].kind, .image)
        XCTAssertEqual(registry.models[1].imageSize, 512)
    }

    func testEmptyObjectDecodes() throws {
        let registry = try JSONDecoder().decode(ModelRegistry.self, from: Data("{}".utf8))
        XCTAssertTrue(registry.models.isEmpty)
        XCTAssertTrue(registry.settings.features.images)
    }
}
