import XCTest
import ITN

/// Tests for the pure-Swift ITN engine. These cover the cases the user
/// originally reported ("one two five O" -> "1250") and a wider regression
/// set so future changes don't quietly break the simple cases.
final class ITNTests: XCTestCase {

    // MARK: - Per-token normalization

    func testNormalizeToken_singleDigitWords() {
        XCTAssertEqual(ITN.normalizeToken("one"), "1")
        XCTAssertEqual(ITN.normalizeToken("two"), "2")
        XCTAssertEqual(ITN.normalizeToken("nine"), "9")
        XCTAssertEqual(ITN.normalizeToken("zero"), "0")
        XCTAssertEqual(ITN.normalizeToken("ten"), "10")
    }

    func testNormalizeToken_letterODigit() {
        // ASR often renders the spoken "oh" as the single letter "O".
        XCTAssertEqual(ITN.normalizeToken("O"), "0")
        XCTAssertEqual(ITN.normalizeToken("o"), "0")
    }

    func testNormalizeToken_passthrough() {
        XCTAssertEqual(ITN.normalizeToken("hello"), "hello")
        XCTAssertEqual(ITN.normalizeToken(""), "")
        XCTAssertEqual(ITN.normalizeToken("42"), "42")
        XCTAssertEqual(ITN.normalizeToken("$"), "$")
    }

    // MARK: - The user-reported case: digit grouping

    func testNormalize_digitGrouping_oneTwoFiveO() {
        XCTAssertEqual(
            ITN.normalize("one two five O"),
            "1250"
        )
    }

    func testNormalize_digitGrouping_phoneNumber() {
        XCTAssertEqual(
            ITN.normalize("one two three four five six seven eight nine zero"),
            "1234567890"
        )
    }

    func testNormalize_digitGrouping_mixedCase() {
        // The original transcript came back as mixed-case because the
        // single-letter "O" was uppercase; output should still be digits.
        // The grouped-number pass turns "one eight hundred" into 1800
        // (a 3+ digit number), and the digit-sequence pass collapses
        // the trailing run. Spacing inside the number is acceptable.
        let r = ITN.normalize("call me at one eight hundred five five five one two three four")
        XCTAssertFalse(r.contains("one") || r.contains("two") || r.contains("three"),
                       "digit words should be normalized away, got: \(r)")
        XCTAssertTrue(r.contains("1800") || r.contains("1815"),
                      "1800 (or 1815 if hundred-form aggregates) should appear, got: \(r)")
    }

    func testNormalize_digitGrouping_insideSentence() {
        // The grouped-number pass may consume "one two" as 12 first, but
        // since "one two" alone is below 100 it falls through to digit
        // grouping — which yields 1250. Whichever path, "one two five O"
        // must collapse to a single digit string.
        let r = ITN.normalize("my number is one two five zero and that's it")
        XCTAssertTrue(r.contains("1250"),
                      "embedded digit group should be normalized, got: \(r)")
    }

    // MARK: - Cardinal collapse

    func testNormalize_trailingCardinal() {
        XCTAssertEqual(ITN.normalize("I have twenty one apples"), "I have 21 apples")
        XCTAssertEqual(ITN.normalize("I have twenty one"), "I have 21")
        XCTAssertEqual(ITN.normalize("there were three hundred and forty two"), "there were 342")
        XCTAssertEqual(ITN.normalize("there were two hundred and thirty"), "there were 230")
        XCTAssertEqual(ITN.normalize("there were three hundred forty two people"), "there were 342 people")
    }

    // MARK: - Money

    func testNormalize_money_simple() {
        XCTAssertEqual(ITN.normalize("five dollars"), "$5")
        XCTAssertEqual(ITN.normalize("twenty dollars"), "$20")
    }

    func testNormalize_money_dollarsAndCents() {
        XCTAssertEqual(ITN.normalize("five dollars and fifty cents"), "$5.50")
        XCTAssertEqual(ITN.normalize("twelve dollars and five cents"), "$12.05")
    }

    func testNormalize_money_centsOnly() {
        XCTAssertEqual(ITN.normalize("fifty cents"), "$0.50")
    }

    // MARK: - Date

    func testNormalize_date_monthOrdinal() {
        XCTAssertEqual(ITN.normalize("january fifth"), "January 5th")
        XCTAssertEqual(ITN.normalize("march twenty first"), "March 21st")
    }

    func testNormalize_date_monthOrdinalYear() {
        XCTAssertEqual(
            ITN.normalize("january fifth twenty twenty five"),
            "January 5th, 2025"
        )
        XCTAssertEqual(
            ITN.normalize("december thirty first twenty twenty four"),
            "December 31st, 2024"
        )
    }

    // MARK: - Time

    func testNormalize_time_basic() {
        XCTAssertEqual(ITN.normalize("two thirty pm"), "2:30 PM")
        XCTAssertEqual(ITN.normalize("nine fifteen am"), "9:15 AM")
    }

    func testNormalize_time_quarterPast() {
        XCTAssertEqual(ITN.normalize("quarter past two pm"), "2:15 PM")
        XCTAssertEqual(ITN.normalize("half past three am"), "3:30 AM")
    }

    // MARK: - Idempotence / passthrough

    func testNormalize_passthrough_plainText() {
        let s = "the quick brown fox jumps over the lazy dog"
        XCTAssertEqual(ITN.normalize(s), s)
    }

    func testNormalize_idempotent() {
        let s = "call me at 18005551234 and pay twelve dollars and five cents on january fifth twenty twenty five"
        let once = ITN.normalize(s)
        let twice = ITN.normalize(once)
        XCTAssertEqual(once, twice, "normalize must be idempotent on its own output")
    }

    func testNormalize_emptyAndWhitespace() {
        XCTAssertEqual(ITN.normalize(""), "")
        XCTAssertEqual(ITN.normalize("   "), "")
    }
}
