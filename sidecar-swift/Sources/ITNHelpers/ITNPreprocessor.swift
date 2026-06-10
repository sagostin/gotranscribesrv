import Foundation
import FluidAudio

/// Pre-processes text before ITN to fix a known limitation of
/// `TextNormalizer.normalizeSentence`: the sentence-mode tagger list
/// **excludes the telephone tagger** (upstream `text-processing-rs`
/// excludes it because it over-fires on natural language). As a result,
/// a 10-digit spoken phone number like "two five zero eight five nine
/// one five zero one" gets mangled by the time/cardinal taggers:
///
///     before: "Hi, my phone number is two five zero eight five nine one five zero one"
///     after : "Hi, my phone number is 2508 5915 1"  ← wrong
///
/// The single-expression `normalize()` DOES include the telephone tagger
/// and produces the correct "250-859-1501" output. The fix: detect runs
/// of 3+ spoken digit-words in the text, route them through `normalize()`
/// instead, then splice the result back into the original sentence with
/// the original spacing/punctuation preserved.
///
/// Edge cases handled:
///   - The run is bounded by non-digit tokens (whitespace, punctuation,
///     ASR connective words like "is", "and", "my"). We use the original
///     whitespace and joiner between runs.
///   - Short runs (1-2 digit words) are NOT pre-processed — they could
///     be money, age, time, count, etc. and the sentence-mode cardinal
///     tagger handles them well.
///   - The single-expression fallback (when the native lib isn't linked)
///     is a passthrough, so pre-processing is safe to enable globally.
public enum ITNPreprocessor {

    /// Spoken digit-words recognized by NeMo ITN. Includes the NATO
    /// "zero" and the ASR form "oh" / "O" for zero. Lowercase comparison.
    /// Kept in sync with what `nemo_normalize` accepts in v0.2.2.
    private static let digitWords: Set<String> = [
        "zero", "oh", "o",
        "one", "two", "three", "four", "five",
        "six", "seven", "eight", "nine",
    ]

    /// Minimum run length (in digit-words) to be considered a phone-number
    /// candidate. 3 covers 7-digit, 10-digit, and 11-digit (1+area+number)
    /// North American numbers. Below this, the sentence-mode cardinal
    /// tagger's behavior is preferred (it understands context for things
    /// like "I have twenty one").
    private static let minRunLength = 3

    /// Detects runs of `minRunLength`+ consecutive spoken digit-words,
    /// replaces each with the result of `textNormalizer.normalize(run)`,
    /// and returns the rewritten text. Everything else is left untouched
    /// (sentence-mode ITN is then applied on top by the caller via
    /// `normalizeSentence`).
    public static func preprocessPhoneNumbers(
        _ text: String,
        normalizer: TextNormalizer
    ) -> String {
        guard !text.isEmpty else { return text }

        // Tokenize on whitespace, preserving the original separators.
        // We don't use `split(separator:)` because we need to keep the
        // exact whitespace tokens to reconstruct the text byte-for-byte.
        let tokens = tokenizePreservingSeparators(text)

        // Group consecutive digit-word tokens into runs. A "digit run" is
        // a contiguous span of digit-words possibly separated by
        // whitespace tokens (`.separator`). Any non-digit `.word` token
        // (i.e. a regular word) breaks the run.
        //
        // We emit each run as a `[Token]` containing the digit words and
        // any INTERNAL whitespace between them. We deliberately EXCLUDE
        // the trailing separator (if any) so the run replacement doesn't
        // eat the space that originally separated the digit run from the
        // next non-digit word.
        var runs: [[Token]] = []
        var currentRun: [Token] = []  // digits + internal whitespace
        var currentWordCount = 0      // how many digit-words in currentRun

        func flushRun() {
            // Trim trailing separators before flushing.
            while let last = currentRun.last, case .separator = last {
                currentRun.removeLast()
            }
            if currentWordCount >= minRunLength {
                runs.append(currentRun)
            }
            currentRun = []
            currentWordCount = 0
        }

        for token in tokens {
            switch token {
            case .word(let w):
                // Strip leading whitespace for the digit-word check;
                // the whitespace stays in the run for reconstruction.
                let stripped = w.drop(while: { $0.isWhitespace })
                if isDigitWord(String(stripped)) {
                    currentRun.append(token)
                    currentWordCount += 1
                } else {
                    // Non-digit word breaks the run.
                    flushRun()
                }
            case .separator:
                // Punctuation between non-digit tokens is dropped (it
                // already appears as part of the non-digit word's
                // prefix-free representation in the original text).
                // Whitespace inside a digit run is part of the run.
                if currentWordCount > 0 {
                    currentRun.append(token)
                }
            }
        }
        flushRun()

        guard !runs.isEmpty else { return text }

        // Reconstruct the text, splicing `normalize()` output in place of
        // each digit run. The original whitespace/punctuation between
        // tokens is preserved.
        var result = ""
        var runIndex = 0
        var i = 0
        while i < tokens.count {
            if runIndex < runs.count {
                let run = runs[runIndex]
                // Check if the current token sequence starts with this run
                let slice = Array(tokens[i..<min(i + run.count, tokens.count)])
                if slice.elementsEqual(run, by: { (a, b) in tokensEqual(a, b) }) {
                    // Extract the leading whitespace of the FIRST digit
                    // word (e.g. " two" -> " ") and the digit-word text
                    // itself. This preserves the space that originally
                    // separated the previous non-digit word from the
                    // run, so the splice doesn't glue tokens together.
                    let words = run.compactMap { token -> String? in
                        if case .word(let w) = token { return w } else { return nil }
                    }
                    guard let first = words.first else { i += run.count; runIndex += 1; continue }
                    let leadingWS = String(first.prefix(while: { $0.isWhitespace }))
                    let strippedWords = words.map { w -> String in
                        String(w.drop(while: { $0.isWhitespace }))
                    }
                    let joined = strippedWords.joined(separator: " ")
                    let normalized = normalizer.normalize(joined)
                    result += leadingWS
                    result += normalized
                    i += run.count
                    runIndex += 1
                    continue
                }
            }
            result += tokenToString(tokens[i])
            i += 1
        }
        return result
    }

    private static func tokensEqual(_ a: Token, _ b: Token) -> Bool {
        switch (a, b) {
        case (.word(let x), .word(let y)): return x == y
        case (.separator(let x), .separator(let y)): return x == y
        default: return false
        }
    }

    // MARK: - Token model

    private enum Token {
        case word(String)
        case separator(String)  // whitespace or punctuation between words
    }

    private static func tokenizePreservingSeparators(_ text: String) -> [Token] {
        // Simple, correct tokenizer: split on whitespace, but each token
        // carries its leading whitespace. Trailing punctuation stays
        // attached to the preceding word. This round-trips perfectly for
        // any input that doesn't have weird mid-word formatting.
        //
        // Tokens look like:
        //   "  hello" -> .word("  hello")
        //   " world" -> .word(" world")
        //   "."     -> .punctuation(".")
        //   ", "    -> .punctuation(", ")
        //
        // (We split punctuation off into its own type so the digit-run
        // detector sees it as a "break", not as part of a word.)
        var tokens: [Token] = []
        var idx = text.startIndex
        while idx < text.endIndex {
            // Skip leading whitespace, then read a word (letters/digits).
            let wordStart = idx
            while idx < text.endIndex, text[idx].isWhitespace {
                idx = text.index(after: idx)
            }
            let wsLen = text.distance(from: wordStart, to: idx)
            if idx == text.endIndex { break }

            // We're at the start of a word or punctuation.
            let tokenStart = idx
            if text[idx].isLetter || text[idx].isNumber {
                while idx < text.endIndex, (text[idx].isLetter || text[idx].isNumber) {
                    idx = text.index(after: idx)
                }
                let word = String(text[tokenStart..<idx])
                // Prepend the leading whitespace if any
                let prefix = wsLen > 0 ? String(text[wordStart..<tokenStart]) : ""
                tokens.append(.word(prefix + word))
            } else {
                // Punctuation: read all consecutive punctuation
                while idx < text.endIndex,
                      !text[idx].isLetter, !text[idx].isNumber, !text[idx].isWhitespace {
                    idx = text.index(after: idx)
                }
                let punct = String(text[tokenStart..<idx])
                // Prepend the leading whitespace if any
                let prefix = wsLen > 0 ? String(text[wordStart..<tokenStart]) : ""
                tokens.append(.separator(prefix + punct))
            }
        }
        return tokens
    }

    private static func tokenToString(_ token: Token) -> String {
        switch token {
        case .word(let w): return w
        case .separator(let s): return s
        }
    }

    private static func isDigitWord(_ w: String) -> Bool {
        digitWords.contains(w.lowercased())
    }
}
