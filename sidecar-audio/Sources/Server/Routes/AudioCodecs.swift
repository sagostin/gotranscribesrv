import Foundation

/// Shared audio codec helpers used by both the legacy `/stream` route and
/// the true-streaming `/stream/realtime` route.

/// G.711 μ-law decode table (ITU-T G.711)
func mulawDecode(_ ulaw: UInt8) -> Int16 {
    // Invert all bits
    let u = ~ulaw
    let sign: Int16 = (u & 0x80) != 0 ? -1 : 1
    let exponent = Int((u >> 4) & 0x07)
    let mantissa = Int(u & 0x0F)

    var magnitude = (mantissa << 4) + 8
    if exponent > 0 {
        magnitude += 0x100
        if exponent > 1 {
            magnitude <<= (exponent - 1)
        }
    }

    return Int16(clamping: sign * Int16(clamping: magnitude))
}

/// G.711 A-law decode table (ITU-T G.711)
func alawDecode(_ alaw: UInt8) -> Int16 {
    let a = alaw ^ 0x55 // Toggle even bits
    let sign: Int16 = (a & 0x80) != 0 ? -1 : 1
    let exponent = Int((a >> 4) & 0x07)
    let mantissa = Int(a & 0x0F)

    var magnitude: Int
    if exponent == 0 {
        magnitude = (mantissa << 4) + 8
    } else {
        magnitude = ((mantissa << 4) + 0x108) << (exponent - 1)
    }

    return Int16(clamping: sign * Int16(clamping: magnitude))
}

/// Simple 2x upsampling (8kHz → 16kHz) via linear interpolation.
func upsample2x(_ input: [Float]) -> [Float] {
    guard input.count > 1 else { return input + input }
    var output = [Float](repeating: 0, count: input.count * 2)
    for i in 0..<input.count {
        output[i * 2] = input[i]
        if i + 1 < input.count {
            output[i * 2 + 1] = (input[i] + input[i + 1]) / 2.0
        } else {
            output[i * 2 + 1] = input[i]
        }
    }
    return output
}
