// SPDX-License-Identifier: MIT
#include "timecode/parser.hpp"

#include <charconv>

#include <fmt/format.h>

#include "timecode/rate_table.hpp"

namespace tc {

bool is_valid_utf8(std::string_view s) noexcept {
    const auto* p = reinterpret_cast<const unsigned char*>(s.data());
    const auto* end = p + s.size();
    while (p < end) {
        unsigned char c = *p;
        std::size_t need = 0;
        unsigned int cp = 0;
        if (c < 0x80) {
            ++p;
            continue;
        } else if ((c & 0xE0) == 0xC0) {
            need = 1;
            cp = c & 0x1F;
            if (cp < 2) return false;  // overlong
        } else if ((c & 0xF0) == 0xE0) {
            need = 2;
            cp = c & 0x0F;
        } else if ((c & 0xF8) == 0xF0) {
            need = 3;
            cp = c & 0x07;
        } else {
            return false;
        }
        ++p;
        for (std::size_t i = 0; i < need; ++i) {
            if (p >= end || (*p & 0xC0) != 0x80) return false;
            cp = (cp << 6) | (*p & 0x3F);
            ++p;
        }
        if (cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF)) return false;
        if (need == 2 && cp < 0x800) return false;  // overlong
        if (need == 3 && cp < 0x10000) return false;
    }
    return true;
}

std::string_view trim_ascii(std::string_view s) noexcept {
    auto is_ws = [](char c) {
        return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f';
    };
    while (!s.empty() && is_ws(s.front())) s.remove_prefix(1);
    while (!s.empty() && is_ws(s.back())) s.remove_suffix(1);
    return s;
}

namespace {

ParseResult fail(std::string raw, Diag d, std::string msg) {
    ParseResult r;
    r.ok = false;
    r.raw = std::move(raw);
    r.diag = d;
    r.message = std::move(msg);
    return r;
}

bool two_digits(std::string_view tok, int& out) {
    if (tok.size() != 2) return false;
    for (char c : tok) if (c < '0' || c > '9') return false;
    out = (tok[0] - '0') * 10 + (tok[1] - '0');
    return true;
}

}  // namespace

ParseResult parse_timecode(std::string_view text_in, Rate rate) {
    const std::string raw(text_in.data(), text_in.size());
    if (!is_valid_utf8(text_in)) return fail(raw, Diag::BadUtf8, "input is not valid UTF-8");

    std::string_view text = trim_ascii(text_in);
    if (text.empty()) return fail(raw, Diag::EmptyLine, "empty timecode line");

    std::int64_t day = 0;
    if (auto at = text.find('@'); at != std::string_view::npos) {
        std::string_view day_tok = trim_ascii(text.substr(0, at));
        text = trim_ascii(text.substr(at + 1));
        if (day_tok.empty() || day_tok[0] == '-')
            return fail(raw, Diag::NegativeDuration, "day offset must be non-negative");
        if (auto [ptr, ec] = std::from_chars(day_tok.data(), day_tok.data() + day_tok.size(), day);
            ec != std::errc{} || ptr != day_tok.data() + day_tok.size())
            return fail(raw, Diag::BadNumber, "invalid day offset");
    }

    // Locate the frame marker: last ';' or ':'.
    Separator sep = Separator::Colon;
    std::size_t marker = std::string_view::npos;
    for (std::size_t i = text.size(); i-- > 0;) {
        if (text[i] == ';') { sep = Separator::Semicolon; marker = i; break; }
        if (text[i] == ':') { sep = Separator::Colon; marker = i; break; }
        if (text[i] == '.') { sep = Separator::Semicolon; marker = i; break; }  // DF alias
    }
    if (marker == std::string_view::npos)
        return fail(raw, Diag::BadSeparator, "missing frame marker (: or ;)");

    std::string_view head = text.substr(0, marker);
    std::string_view ff = text.substr(marker + 1);

    // Split HH:MM:SS.
    std::vector<std::string_view> parts;
    std::size_t start = 0;
    while (true) {
        auto pos = head.find(':', start);
        parts.push_back(head.substr(start, pos == std::string_view::npos ? std::string_view::npos : pos - start));
        if (pos == std::string_view::npos) break;
        start = pos + 1;
    }
    if (parts.size() != 3)
        return fail(raw, Diag::FieldCount, "expected HH:MM:SS:FF (4 fields)");

    int hh, mm, ss, frame;
    if (!two_digits(parts[0], hh) || !two_digits(parts[1], mm) ||
        !two_digits(parts[2], ss) || !two_digits(ff, frame))
        return fail(raw, Diag::BadNumber, "each field must be exactly two decimal digits");

    if (hh > 23 || mm > 59 || ss > 59)
        return fail(raw, Diag::OutOfRange, "H/M/S component out of range");

    const RateInfo info = rate_info(rate);
    if (frame >= info.nominal_fps)
        return fail(raw, Diag::OutOfRange, "frame number exceeds rate maximum");

    const bool declares_df = (sep == Separator::Semicolon);
    if (declares_df != info.drop_frame)
        return fail(raw, Diag::SeparatorMismatch,
                    info.drop_frame ? "DF rate requires ';' frame marker"
                                    : "NDF rate requires ':' frame marker");

    // Illegal DF landing: frames 00/01 inside a drop minute (minute % 10 != 0).
    if (info.drop_frame && (mm % 10) != 0 && (frame == 0 || frame == 1))
        return fail(raw, Diag::IllegalDropFrame,
                    "frame 00/01 are skipped at this drop-frame minute");

    ParseResult r;
    r.ok = true;
    r.raw = raw;
    r.separator = sep;
    r.tc = Timecode{day, hh, mm, ss, frame};
    return r;
}

ParseResult parse_timecode_auto(std::string_view text) {
    // Infer from the final marker; default NDF when no marker is present
    // (the strict parse then reports the separator error anyway).
    bool saw_semi = false;
    for (char c : text) {
        if (c == ';' || c == '.') { saw_semi = true; break; }
    }
    return parse_timecode(text, saw_semi ? Rate::F2997_DF : Rate::F2997_NDF);
}

std::string format_timecode(const Timecode& tc, Rate rate) {
    const char marker = rate_info(rate).drop_frame ? ';' : ':';
    std::string body = fmt::format("{:02d}:{:02d}:{:02d}{}{:02d}",
                                   tc.hour, tc.minute, tc.second, marker, tc.frame);
    return tc.day != 0 ? fmt::format("{}@{}", tc.day, body) : body;
}

}  // namespace tc
