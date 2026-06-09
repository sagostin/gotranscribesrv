import Foundation

/// Inverse Text Normalization (ITN) — converts spoken-form ASR output to
/// written form. e.g. "one two five O" -> "1250", "five dollars" -> "$5",
/// "january fifth twenty twenty five" -> "January 5, 2025".
///
/// Covers the most common categories that show up in real ASR transcripts:
///
///   - cardinal numbers           "twenty one"      -> "21"
///   - digit-grouped sequences     "one two five O"  -> "1250"
///   - telephone / ID sequences    "one two three four five six seven eight nine zero" -> "1234567890"
///   - year / grouped cardinals    "nineteen fifty five"   -> "1955"
///                                 "one eight hundred oh five" -> "1805"
///   - ordinal numbers             "january fifth"   -> "January 5th"
///                                 "december thirty first" -> "December 31st"
///   - money                       "five dollars"    -> "$5"
///                                 "five dollars and fifty cents" -> "$5.50"
///   - time                        "two thirty pm"   -> "2:30 PM"
///                                 "quarter past two pm" -> "2:15 PM"
///   - punctuation/case            don't touch
///
/// Strategy: tokenize on whitespace, walk left-to-right, recognize each
/// "chunk" (a sequence of digit-words, a money phrase, a month + ordinal,
/// etc.) and replace it with the written form. Unknown words are passed
/// through unchanged so we never damage free text.
///
/// Thread safety: pure value semantics, no shared state. Safe to call from
/// any actor/thread.
public enum ITN {

    // MARK: - Public API

    /// Normalize a single token. Returns the input unchanged if no rule
    /// applies. Use this in token-stream consumers (e.g. inside the
    /// FluidAudio token timing loop) to fix per-token cases.
    public static func normalizeToken(_ token: String) -> String {
        let t = token.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty else { return token }

        // Already a number / punctuation -> leave it
        if isNumeric(t) { return t }

        let lower = t.lowercased()
        if let n = cardinal[lower] {
            return "\(n)"
        }
        // Letter->digit for single letters (ASR says "oh" as "O")
        if t.count == 1, let c = t.first, let d = letterDigit[c] {
            return String(d)
        }
        return t
    }

    /// Normalize an entire utterance. Handles multi-word phrases that
    /// collapse to a single written form (digit sequences, money, dates).
    public static func normalize(_ text: String) -> String {
        guard !text.isEmpty else { return text }

        let words = text.split(separator: " ", omittingEmptySubsequences: true)
        var out: [String] = []
        out.reserveCapacity(words.count)
        var i = 0
        while i < words.count {
            // Money: "5 dollars and 50 cents", "5 dollars", "50 cents"
            if let (chunk, consumed) = matchMoney(words: words, startAt: i) {
                out.append(chunk)
                i += consumed
                continue
            }

            // Date: "january fifth twenty twenty five"
            if let (chunk, consumed) = matchDate(words: words, startAt: i) {
                out.append(chunk)
                i += consumed
                continue
            }

            // Time: "two thirty pm", "quarter past two pm", "half past three"
            if let (chunk, consumed) = matchTime(words: words, startAt: i) {
                out.append(chunk)
                i += consumed
                continue
            }

            // Year / grouped cardinal: "nineteen fifty five" -> 1955
            //                          "one eight hundred" -> 1800
            //                          "one eight hundred oh five" -> 1805
            //                          "twenty twenty five" -> 2025
            if let (chunk, consumed) = matchGroupedNumber(words: words, startAt: i) {
                out.append(chunk)
                i += consumed
                continue
            }

            // Digit-only sequence: a run of digit-words / O/zero. This is the
            // "one two five O" -> "1250" case (and phone numbers, IDs, etc.)
            if let (chunk, consumed) = matchDigitSequence(words: words, startAt: i) {
                out.append(chunk)
                i += consumed
                continue
            }

            out.append(String(words[i]))
            i += 1
        }

        var joined = out.joined(separator: " ")

        // Post-pass: collapse any embedded cardinal run (1-5 words, possibly
        // with "and" inside) to digits. Handles "I have twenty one apples"
        // -> "I have 21 apples" and "there were three hundred and forty two
        // people" -> "there were 342 people".
        joined = collapseEmbeddedCardinalRuns(joined)

        // Tidy double spaces that may have been introduced.
        while joined.contains("  ") {
            joined = joined.replacingOccurrences(of: "  ", with: " ")
        }
        return joined.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    // MARK: - Digit sequence ("one two five O" -> "1250")

    private static func matchDigitSequence(words: [Substring], startAt i: Int) -> (String, Int)? {
        var digits: [String] = []
        var j = i
        while j < words.count {
            let w = String(words[j]).lowercased()
            if let d = digitWord[w] {
                digits.append(d)
            } else if w.count == 1, let c = w.first, let d = letterDigit[c] {
                digits.append(String(d))
            } else {
                break
            }
            j += 1
            if digits.count >= 16 { break }
        }
        guard digits.count >= 2 else { return nil }
        return (digits.joined(), j - i)
    }

    // MARK: - Grouped cardinal / year

    /// Handles patterns where the ASR model groups digits with a "hundred"
    /// marker (the classic NeMo ITN rule for years and phone numbers):
    ///   "nineteen fifty five"           -> 1955
    ///   "one eight hundred"             -> 1800
    ///   "one eight hundred oh five"     -> 1805
    ///   "twenty twenty five"            -> 2025
    ///   "two thousand and five"         -> 2005
    ///   "one thousand nine hundred ninety nine" -> 1999
    ///
    /// We only fire when the result is a 3+ digit number and the run is
    /// at least 3 words long, so we don't accidentally rewrite short
    /// cardinals ("twenty one" stays for the trailing-run pass).
    private static func matchGroupedNumber(words: [Substring], startAt i: Int) -> (String, Int)? {
        let maxWindow = min(6, words.count - i)
        guard maxWindow >= 3 else { return nil }

        let parts = (0..<maxWindow).map { String(words[i + $0]).lowercased() }

        // Try the longest reasonable window first: up to 6 words.
        for window in (3...maxWindow).reversed() {
            let slice = Array(parts.prefix(window))
            if let n = parseGrouped(slice), n >= 100 {
                return ("\(n)", window)
            }
        }
        return nil
    }

    private static func parseGrouped(_ parts: [String]) -> Int? {
        // "two thousand and five" / "two thousand five"
        if let n = parseThousandAnd(parts) { return n }

        // "X hundred [(and) Z]" / "X Y hundred [(and) Z]"
        if let n = parseHundredForm(parts) { return n }

        // "X Y" where X in {nineteen, twenty} and Y < 100 -> year
        if parts.count == 2,
           let a = cardinal[parts[0]], let b = cardinal[parts[1]],
           (19...20).contains(a), b < 100 {
            return a * 100 + b
        }

        return nil
    }

    private static func parseThousandAnd(_ parts: [String]) -> Int? {
        // "two thousand [and] [five|ninety nine|...]"
        guard parts.count >= 2 else { return nil }
        guard cardinal[parts[0]] == 2000 else { return nil }
        var idx = 1
        if parts[idx] == "and" { idx += 1 }
        let rest = Array(parts[idx...])
        guard !rest.isEmpty, rest.allSatisfy({ cardinal[$0] != nil }) else { return nil }
        guard let tail = parseSimpleCardinal(rest) else { return nil }
        return 2000 + tail
    }

    private static func parseHundredForm(_ parts: [String]) -> Int? {
        // "X hundred"          -> X * 100
        // "X Y hundred"        -> XY * 100  (e.g. one eight hundred = 1800)
        // "X hundred [(and) Z]" -> X * 100 + Z
        // "X Y hundred [(and) Z]" -> XY * 100 + Z
        guard let hundredIdx = parts.firstIndex(of: "hundred") else { return nil }

        let prefix = Array(parts[0..<hundredIdx])
        let suffix = Array(parts[(hundredIdx + 1)...])

        // "and <Z>" -> drop the and
        var tail: [String] = []
        if let first = suffix.first, first == "and" {
            tail = Array(suffix.dropFirst())
        } else {
            tail = suffix
        }
        guard tail.isEmpty || tail.allSatisfy({ cardinal[$0] != nil }) else { return nil }

        // Compute the leading digits (prefix).
        let leading: Int
        if prefix.isEmpty {
            leading = 1  // bare "hundred" = 100
        } else if prefix.count == 1, let v = cardinal[prefix[0]] {
            leading = v
        } else if prefix.count == 2,
                  let a = cardinal[prefix[0]], let b = cardinal[prefix[1]],
                  a < 10, b < 10 {
            leading = a * 10 + b  // "one eight" -> 18
        } else {
            return nil
        }

        let hundreds = leading * 100
        if tail.isEmpty {
            return hundreds
        }
        guard let minor = parseSimpleCardinal(tail) else { return nil }
        return hundreds + minor
    }

    /// Parse a "simple" cardinal (no "hundred"/"thousand") like
    /// "twenty one" / "forty two" / "ninety nine".
    private static func parseSimpleCardinal(_ parts: [String]) -> Int? {
        var current = 0
        for p in parts {
            guard let v = cardinal[p] else { return nil }
            if v >= 100 { return nil }
            current += v
        }
        return current
    }

    // MARK: - Money

    private static func matchMoney(words: [Substring], startAt i: Int) -> (String, Int)? {
        if i + 1 >= words.count { return nil }

        let w0 = String(words[i]).lowercased()

        // "<n> dollars and <m> cents"  (consume 5 words including "cents")
        if i + 4 < words.count {
            let w1 = String(words[i+1]).lowercased()
            let w2 = String(words[i+2]).lowercased()
            let w3 = String(words[i+3]).lowercased()
            let w4 = String(words[i+4]).lowercased()
            if w1 == "dollars" && w2 == "and" && (w4 == "cents" || w4 == "cent") {
                if let major = cardinal[w0], let minor = cardinal[w3] {
                    return (String(format: "$%d.%02d", major, minor), 5)
                }
            }
        }
        // "<n> dollars" / "<n> dollar"
        if i + 1 < words.count {
            let w1 = String(words[i+1]).lowercased()
            if w1 == "dollars" || w1 == "dollar" {
                if let major = cardinal[w0] {
                    return (String(format: "$%d", major), 2)
                }
            }
            // "<n> cents" / "<n> cent"
            if w1 == "cents" || w1 == "cent" {
                if let minor = cardinal[w0] {
                    return (String(format: "$0.%02d", minor), 2)
                }
            }
        }
        return nil
    }

    // MARK: - Date

    private static let monthNames: [String: String] = [
        "january": "January", "february": "February", "march": "March",
        "april": "April", "may": "May", "june": "June",
        "july": "July", "august": "August", "september": "September",
        "october": "October", "november": "November", "december": "December",
    ]

    private static func matchDate(words: [Substring], startAt i: Int) -> (String, Int)? {
        guard i + 1 < words.count else { return nil }
        let m = String(words[i]).lowercased()
        guard let month = monthNames[m] else { return nil }

        // "<month> <ordinal> [year-words]"
        // The ordinal may be 1 word ("fifth") or a 2-word compound
        // ("twenty first", "thirty first").
        let ord1 = String(words[i+1]).lowercased()
        let day: Int
        var ordConsumed: Int
        if let v = ordinalNumber(ord1) {
            day = v
            ordConsumed = 1
        } else if i + 2 < words.count {
            let ord2 = String(words[i+2]).lowercased()
            let pair = "\(ord1) \(ord2)"
            if let v = ordinalNumber(pair) {
                day = v
                ordConsumed = 2
            } else {
                return nil
            }
        } else {
            return nil
        }
        let dayStr = ordinalSuffix(day)

        // Try to consume a 4-digit year expressed as words.
        let yearStart = i + 1 + ordConsumed
        if yearStart < words.count {
            if let (year, consumed) = matchYear(words: words, startAt: yearStart) {
                return ("\(month) \(dayStr), \(year)", 1 + ordConsumed + consumed)
            }
        }
        return ("\(month) \(dayStr)", 1 + ordConsumed)
    }

    private static func matchYear(words: [Substring], startAt i: Int) -> (Int, Int)? {
        // "two thousand and five" / "two thousand five"
        if i + 2 < words.count {
            let a = String(words[i]).lowercased()
            let b = String(words[i+1]).lowercased()
            if let av = cardinal[a], av == 2000, b == "and",
               let cv = cardinal[String(words[i+2]).lowercased()] {
                return (av + cv, 3)
            }
        }
        // "twenty twenty five" (3-word form, e.g. 2025)
        if i + 2 < words.count {
            let a = String(words[i]).lowercased()
            let b = String(words[i+1]).lowercased()
            let c = String(words[i+2]).lowercased()
            if let av = cardinal[a], (19...20).contains(av),
               let bv = cardinal[b], (20...29).contains(bv),
               let cv = cardinal[c], cv < 10 {
                return (av * 100 + bv + cv, 3)
            }
        }
        // "nineteen fifty five" / "twenty twenty"
        if i + 1 < words.count {
            let a = String(words[i]).lowercased()
            let b = String(words[i+1]).lowercased()
            if let av = cardinal[a], let bv = cardinal[b] {
                if (19...20).contains(av) && bv < 100 {
                    return (av * 100 + bv, 2)
                }
            }
        }
        return nil
    }

    // MARK: - Time

    private static func matchTime(words: [Substring], startAt i: Int) -> (String, Int)? {
        // "quarter past two pm" / "half past three am"
        if i + 3 < words.count {
            let w0 = String(words[i]).lowercased()
            let w1 = String(words[i+1]).lowercased()
            let w2 = String(words[i+2]).lowercased()
            let w3 = String(words[i+3]).lowercased()
            if (w0 == "quarter" || w0 == "half") && w1 == "past" {
                if let h = cardinal[w2] {
                    let suffix = normalizeAMPM(w3) ?? ""
                    let minute = w0 == "quarter" ? 15 : 30
                    return ("\(h):" + String(format: "%02d", minute) + (suffix.isEmpty ? "" : " \(suffix)"), 4)
                }
            }
        }

        // "two thirty pm" / "nine fifteen am"
        if i + 2 < words.count {
            let w0 = String(words[i]).lowercased()
            let w1 = String(words[i+1]).lowercased()
            if let h = cardinal[w0], let m = cardinal[w1], m < 60 {
                let w2 = String(words[i+2]).lowercased()
                if let s = normalizeAMPM(w2) {
                    return (String(format: "%d:%02d %@", h, m, s), 3)
                }
            }
        }
        return nil
    }

    private static func normalizeAMPM(_ s: String) -> String? {
        switch s.lowercased() {
        case "am", "a.m.", "a m": return "AM"
        case "pm", "p.m.", "p m": return "PM"
        default: return nil
        }
    }

    // MARK: - Cardinal run collapse

    /// Find every embedded cardinal run (a run of 1-5 words that, ignoring
    /// `and`, all resolve to cardinal values) and rewrite it as digits.
    /// Words already in digit form are left alone.
    ///
    /// Examples:
    ///   "I have twenty one apples"     -> "I have 21 apples"
    ///   "three hundred forty two"     -> "342"
    ///   "I paid twenty dollars"       -> "I paid 20 dollars"
    ///   "there were 5 people"         -> "there were 5 people"  (no change)
    private static func collapseEmbeddedCardinalRuns(_ text: String) -> String {
        let words = text.split(separator: " ", omittingEmptySubsequences: true)
        guard words.count >= 1 else { return text }

        var out: [String] = []
        out.reserveCapacity(words.count)
        var i = 0
        while i < words.count {
            // Try the longest run starting at i (up to 5 words).
            var matched = false
            for window in (1...5).reversed() {
                guard i + window <= words.count else { continue }
                let slice = (0..<window).map { String(words[i + $0]).lowercased() }
                // Every token must be a cardinal component (or "and").
                if slice.contains(where: { $0 == "and" }) {
                    // Allow at most one "and" and it must be interior.
                    let andCount = slice.filter { $0 == "and" }.count
                    guard andCount == 1 else { continue }
                    let andIdx = slice.firstIndex(of: "and")!
                    guard andIdx > 0 && andIdx < slice.count - 1 else { continue }
                }
                let cardinals = slice.filter { $0 != "and" }
                guard !cardinals.isEmpty,
                      cardinals.allSatisfy({ cardinal[$0] != nil }) else { continue }
                if let n = parseCompoundCardinal(slice) {
                    out.append(String(n))
                    i += window
                    matched = true
                    break
                }
            }
            if !matched {
                out.append(String(words[i]))
                i += 1
            }
        }
        return out.joined(separator: " ")
    }

    /// "twenty one" / "two hundred and thirty" / "three hundred and forty two"
    private static func parseCompoundCardinal(_ parts: [String]) -> Int? {
        var total = 0
        var current = 0
        var saw = false
        for p in parts {
            guard let v = cardinal[p] else { return nil }
            saw = true
            if v == 100 {
                current = (current == 0 ? 1 : current) * 100
                total += current
                current = 0
            } else if v >= 1000 {
                let base = current == 0 ? 1 : current
                total += base * v
                current = 0
            } else {
                current += v
            }
        }
        if !saw { return nil }
        return total + current
    }

    // MARK: - Helpers

    private static func isNumeric(_ s: String) -> Bool {
        var seenDigit = false
        for c in s {
            if c.isNumber { seenDigit = true; continue }
            if c == "." || c == "," || c == "-" { continue }
            return false
        }
        return seenDigit
    }

    private static func ordinalNumber(_ word: String) -> Int? {
        // Direct match first.
        if let v = ordinalSpoken[word.lowercased()] { return v }
        // Compound ordinals: "<tens> <unit>"  e.g. "thirty first" -> 31,
        //                     "twenty first"   -> 21, "forty second" -> 42.
        let parts = word.lowercased().split(separator: " ").map(String.init)
        if parts.count == 2,
           let tens = tensMap[parts[0]], let unit = ordinalUnits[parts[1]] {
            return tens + unit
        }
        return nil
    }

    private static func ordinalSuffix(_ n: Int) -> String {
        let mod100 = n % 100
        let mod10 = n % 10
        if (11...13).contains(mod100) { return "\(n)th" }
        switch mod10 {
        case 1: return "\(n)st"
        case 2: return "\(n)nd"
        case 3: return "\(n)rd"
        default: return "\(n)th"
        }
    }

    // MARK: - Tables

    /// cardinal: spoken -> numeric.
    /// 0..19, then tens, then hundreds/thousands.
    private static let cardinal: [String: Int] = [
        "zero": 0, "oh": 0, "o": 0,
        "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
        "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
        "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14,
        "fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19,
        "twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
        "sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
        "hundred": 100, "thousand": 1000, "million": 1_000_000, "billion": 1_000_000_000,
    ]

    private static let digitWord: [String: String] = [
        "zero": "0", "oh": "0", "o": "0",
        "one": "1", "two": "2", "three": "3", "four": "4", "five": "5",
        "six": "6", "seven": "7", "eight": "8", "nine": "9",
    ]

    private static let letterDigit: [Character: Character] = [
        "o": "0", "O": "0",
        "l": "1", "I": "1",
        "z": "2", "Z": "2",
        "s": "5", "S": "5",
        "b": "8", "B": "8",
        "g": "9", "q": "9", "Q": "9",
    ]

    /// ordinal: spoken -> numeric root. The suffix is added by ordinalSuffix().
    private static let ordinalSpoken: [String: Int] = [
        "first": 1, "second": 2, "third": 3, "fourth": 4,
        "fifth": 5, "sixth": 6, "seventh": 7, "eighth": 8,
        "ninth": 9, "tenth": 10, "eleventh": 11, "twelfth": 12,
        "thirteenth": 13, "fourteenth": 14, "fifteenth": 15,
        "sixteenth": 16, "seventeenth": 17, "eighteenth": 18,
        "nineteenth": 19, "twentieth": 20, "thirtieth": 30,
        "fortieth": 40, "fiftieth": 50,
    ]

    /// Tens values for compound ordinals.
    private static let tensMap: [String: Int] = [
        "twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
        "sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
    ]

    /// Unit values for compound ordinals.
    private static let ordinalUnits: [String: Int] = [
        "first": 1, "second": 2, "third": 3, "fourth": 4,
        "fifth": 5, "sixth": 6, "seventh": 7, "eighth": 8,
        "ninth": 9,
    ]
}
