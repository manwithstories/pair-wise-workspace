#include "subqc/inline_tags.hpp"

#include <algorithm>
#include <cctype>
#include <set>
#include <string>
#include <vector>

#include "subqc/ass_reader.hpp"

namespace subqc {
namespace {

const std::set<std::string> &known_tags() {
    static const std::set<std::string> tags = {
        "r", "b", "i", "u", "s", "fs", "fn", "fscx", "fscy", "frx", "fry", "frz", "fe",
        "pos", "move", "org", "an", "a", "fad", "fade", "t", "clip", "iclip", "blur", "be",
        "shad", "bord", "xbord", "ybord", "xshad", "yshad", "1c", "2c", "3c", "4c", "alpha",
        "1a", "2a", "3a", "4a", "p", "pbo", "q", "k", "K"};
    return tags;
}

std::string lower_ascii(std::string value) {
    std::transform(value.begin(), value.end(), value.begin(),
                   [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
    return value;
}

int utf8_glyphs(const std::string &text) {
    int count = 0;
    for (std::size_t i = 0; i < text.size();) {
        const unsigned char ch = static_cast<unsigned char>(text[i]);
        if (ch == '\\' && i + 1 < text.size()) {
            const char next = text[i + 1];
            if (next == 'N' || next == 'n' || next == 'h') {
                i += 2;
                continue;
            }
        }
        if (ch <= 0x20) {
            ++i;
            continue;
        }
        ++count;
        if (ch < 0x80) {
            ++i;
        } else if ((ch & 0xE0) == 0xC0) {
            i += 2;
        } else if ((ch & 0xF0) == 0xE0) {
            i += 3;
        } else if ((ch & 0xF8) == 0xF0) {
            i += 4;
        } else {
            ++i;
        }
    }
    return count;
}

void flush_segment(InlineAnalysis &analysis, std::string &buffer, const std::string &style) {
    if (buffer.empty()) return;
    const int glyphs = utf8_glyphs(buffer);
    if (!analysis.segments.empty() && analysis.segments.back().style == style) {
        analysis.segments.back().text += buffer;
        analysis.segments.back().glyphs += glyphs;
    } else {
        analysis.segments.push_back({buffer, style, glyphs});
    }
    analysis.glyphs += glyphs;
    buffer.clear();
}

std::size_t find_closing_paren(const std::string &text, std::size_t open) {
    int depth = 0;
    for (std::size_t i = open; i < text.size(); ++i) {
        if (text[i] == '(') ++depth;
        else if (text[i] == ')') {
            --depth;
            if (depth == 0) return i;
        }
    }
    return std::string::npos;
}

void parse_tags(const std::string &content, bool animation, const std::string &base_style,
                const StyleCatalog &catalog, const std::string &file, int line,
                std::string &current_style, InlineAnalysis &analysis);

void apply_tag(const std::string &name, const std::string &value, bool animation,
               const std::string &base_style, const StyleCatalog &catalog,
               const std::string &file, int line, std::string &current_style,
               InlineAnalysis &analysis) {
    if (name == "r" && !animation) {
        const std::string requested = trim_copy(value);
        if (requested.empty()) {
            current_style = base_style;
            return;
        }
        const auto found = catalog.resolved.find(lower_copy(requested));
        if (found == catalog.resolved.end() || !found->second.valid) {
            analysis.diagnostics.push_back({"warning", file, line, "missing_style_ref",
                                            "\\r references missing or invalid style '" + requested + "'"});
            current_style = base_style;
        } else {
            current_style = found->second.name;
        }
    }
}

void parse_tags(const std::string &content, bool animation, const std::string &base_style,
                const StyleCatalog &catalog, const std::string &file, int line,
                std::string &current_style, InlineAnalysis &analysis) {
    std::size_t i = 0;
    while (i < content.size()) {
        const auto slash = content.find('\\', i);
        if (slash == std::string::npos) break;
        i = slash + 1;
        if (i >= content.size()) break;
        std::string name;
        const char first = content[i];
        if (first == 'r' || first == 't') {
            name = std::string(1, first);
            ++i;
        } else {
            const std::size_t begin = i;
            if (std::isdigit(static_cast<unsigned char>(content[i]))) {
                while (i < content.size() && std::isalnum(static_cast<unsigned char>(content[i]))) ++i;
            } else {
                while (i < content.size() && std::isalpha(static_cast<unsigned char>(content[i]))) ++i;
            }
            name = content.substr(begin, i - begin);
            if (name.empty()) {
                name = std::string(1, first);
                ++i;
            }
        }
        if (name.empty()) continue;
        std::string value;
        if (i < content.size() && content[i] == '(') {
            const std::size_t close = find_closing_paren(content, i);
            if (close == std::string::npos) {
                analysis.malformed = true;
                analysis.diagnostics.push_back({"warning", file, line, "unclosed_tag_paren",
                                                "Unclosed parenthesis in override tag"});
                value = content.substr(i + 1);
                i = content.size();
            } else {
                value = content.substr(i + 1, close - i - 1);
                i = close + 1;
            }
        } else {
            const std::size_t begin = i;
            while (i < content.size() && content[i] != '\\') ++i;
            value = trim_copy(content.substr(begin, i - begin));
        }
        const std::string normalized = lower_ascii(name);
        ++analysis.tag_counts[normalized];
        if (!known_tags().count(normalized)) ++analysis.unknown_tag_counts[normalized];
        if (normalized == "t") {
            parse_tags(value, true, base_style, catalog, file, line, current_style, analysis);
        } else {
            apply_tag(normalized, value, animation, base_style, catalog, file, line, current_style,
                      analysis);
        }
    }
}

}  // namespace

InlineAnalysis analyze_inline_tags(const std::string &text, const std::string &base_style,
                                   const StyleCatalog &catalog, const std::string &file, int line) {
    InlineAnalysis analysis;
    std::string current_style = base_style;
    std::string plain;
    for (std::size_t i = 0; i < text.size();) {
        if (text[i] == '\\' && i + 1 < text.size() && text[i + 1] == '{') {
            plain.push_back('{');
            i += 2;
            continue;
        }
        if (text[i] != '{') {
            plain.push_back(text[i++]);
            continue;
        }
        flush_segment(analysis, plain, current_style);
        const auto close = text.find('}', i + 1);
        if (close == std::string::npos) {
            analysis.malformed = true;
            analysis.diagnostics.push_back({"warning", file, line, "unclosed_override",
                                            "Override block is missing closing brace"});
            plain.append(text.substr(i));
            break;
        }
        parse_tags(text.substr(i + 1, close - i - 1), false, base_style, catalog, file, line,
                   current_style, analysis);
        i = close + 1;
    }
    flush_segment(analysis, plain, current_style);
    return analysis;
}

}  // namespace subqc
