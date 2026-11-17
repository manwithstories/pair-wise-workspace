// SPDX-License-Identifier: MIT
// timecode/parser.hpp — strict SMPTE-style timecode parsing.
//
// Accepted grammar:
//   [DAY "@"] HH ":" MM ":" SS (":" | ";") FF
// DAY is a non-negative integer day offset; FF is exactly two digits.
// ';' (or '.' as a documented DF alias) declares drop-frame, ':' declares
// non-drop; the declared marker must match the target rate.
#pragma once

#include <optional>
#include <string>
#include <string_view>

#include "timecode/types.hpp"

namespace tc {

// True for well-formed UTF-8 (ASCII included). Rejects overlong encodings,
// surrogates and codepoints above U+10FFFF.
bool is_valid_utf8(std::string_view s) noexcept;

// Full strict parse against a known rate. The rate decides the legal frame
// range and the required separator.
ParseResult parse_timecode(std::string_view text, Rate rate);

// Parse while inferring the rate from the separator: ';' -> 2997df,
// ':' -> 2997ndf. Useful when the input stream mixes formats.
ParseResult parse_timecode_auto(std::string_view text);

// Format a timecode back to canonical text (with its day offset when nonzero).
std::string format_timecode(const Timecode& tc, Rate rate);

// Trim ASCII whitespace on both sides (tabs/spaces/CR); UTF-8 interior is
// left untouched.
std::string_view trim_ascii(std::string_view s) noexcept;

}  // namespace tc
