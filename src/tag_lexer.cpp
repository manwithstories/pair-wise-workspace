#include "subtitle_audit/tag_lexer.hpp"

#include <algorithm>
#include <cctype>
#include <set>

namespace audit {
namespace {

const std::set<std::string> &known_tags() {
    static const std::set<std::string> tags = {
        "r", "fs", "bord", "an", "pos", "fad", "t", "fade", "move", "org",
        "b", "i", "u", "s", "strikeout", "fn", "fe", "fr", "frx", "fry", "frz",
        "fax", "fay", "fscx", "fscy", "fsp", "blur", "be", "shad", "xbord",
        "ybord", "xshad", "yshad", "c", "1c", "2c", "3c", "4c", "alpha", "1a",
        "2a", "3a", "4a", "clip", "iclip", "p", "pbo", "q", "k", "ko", "kf",
        "kt", "a", "distort"
    };
    return tags;
}

bool is_alpha(char ch) {
    return std::isalpha(static_cast<unsigned char>(ch)) != 0;
}

bool is_digit(char ch) {
    return std::isdigit(static_cast<unsigned char>(ch)) != 0;
}

std::size_t parse_name_end(const std::string &text, std::size_t begin, std::size_t end) {
    std::size_t pos = begin;
    if (pos < end && is_digit(text[pos])) {
        ++pos;
        while (pos < end && is_alpha(text[pos])) ++pos;
        return pos;
    }
    while (pos < end && is_alpha(text[pos])) ++pos;
    return pos;
}

std::size_t find_argument_end(const std::string &text, std::size_t begin, std::size_t end) {
    int parens = 0;
    for (std::size_t pos = begin; pos < end; ++pos) {
        const char ch = text[pos];
        if (ch == '(') ++parens;
        else if (ch == ')' && parens > 0) --parens;
        else if (ch == '\\' && parens == 0) return pos;
    }
    return end;
}

std::size_t matching_paren(const std::string &text, std::size_t open, std::size_t end) {
    int depth = 0;
    for (std::size_t pos = open; pos < end; ++pos) {
        if (text[pos] == '(') ++depth;
        else if (text[pos] == ')' && --depth == 0) return pos;
    }
    return end;
}

void lex_controls(const std::string &text, std::size_t begin, std::size_t end,
                  std::size_t block_end, int depth, LexedEvent &out) {
    std::size_t pos = begin;
    while (pos < end) {
        if (text[pos] != '\\') { ++pos; continue; }
        const std::size_t slash = pos;
        ++pos;
        if (pos >= end) break;
        std::size_t name_end;
        if (text[pos] == 'r' || text[pos] == 'R') name_end = pos + 1;
        else name_end = parse_name_end(text, pos, end);
        if (name_end == pos) { ++pos; continue; }
        const std::string name = text.substr(pos, name_end - pos);
        const std::size_t arg_end = find_argument_end(text, name_end, end);
        TagToken token;
        token.name = name;
        std::transform(token.name.begin(), token.name.end(), token.name.begin(),
                       [](unsigned char ch) { return static_cast<char>(std::tolower(ch)); });
        token.argument = text.substr(name_end, arg_end - name_end);
        token.raw = text.substr(slash, arg_end - slash);
        token.begin = slash;
        token.end = arg_end;
        token.block_end = block_end;
        token.t_depth = depth;
        out.tags.push_back(token);
        out.max_t_depth = std::max(out.max_t_depth, depth);
        if (token.name == "t") {
            const auto open = text.find('(', name_end);
            if (open != std::string::npos && open < arg_end) {
                const std::size_t close = matching_paren(text, open, arg_end);
                lex_controls(text, open + 1, close, block_end, depth + 1, out);
            }
        }
        pos = arg_end > pos ? arg_end : pos + 1;
    }
}

std::string unescape_text(const std::string &text, std::size_t begin, std::size_t end) {
    std::string visible;
    for (std::size_t pos = begin; pos < end; ++pos) {
        if (text[pos] == '\\' && pos + 1 < end) {
            const char next = text[pos + 1];
            if (next == 'N' || next == 'n') visible.push_back('\n');
            else if (next == 'h') visible.push_back(' ');
            else if (next == '{' || next == '}' || next == '\\') visible.push_back(next);
            else { visible.push_back('\\'); visible.push_back(next); }
            ++pos;
        } else {
            visible.push_back(text[pos]);
        }
    }
    return visible;
}

}  // namespace

bool is_known_tag(const std::string &name) {
    return known_tags().count(name) != 0;
}

LexedEvent lex_event_text(const std::string &text) {
    LexedEvent event;
    std::size_t chunk_begin = 0;
    std::size_t pos = 0;
    while (pos < text.size()) {
        if (text[pos] == '\\' && pos + 1 < text.size() &&
            (text[pos + 1] == '{' || text[pos + 1] == '}')) {
            pos += 2;
            continue;
        }
        if (text[pos] != '{') { ++pos; continue; }
        if (pos > chunk_begin) {
            event.text.push_back({unescape_text(text, chunk_begin, pos), chunk_begin, pos});
        }
        int depth = 1;
        std::size_t scan = pos + 1;
        while (scan < text.size() && depth > 0) {
            if (text[scan] == '\\' && scan + 1 < text.size() &&
                (text[scan + 1] == '{' || text[scan + 1] == '}')) {
                scan += 2;
                continue;
            }
            if (text[scan] == '{') ++depth;
            else if (text[scan] == '}' && --depth == 0) break;
            ++scan;
        }
        const bool closed = scan < text.size() && depth == 0;
        if (!closed) ++event.unclosed_braces;
        const std::size_t content_end = closed ? scan : text.size();
        const std::size_t block_end = closed ? scan + 1 : text.size();
        OverrideBlock block;
        block.begin = pos;
        block.end = block_end;
        block.content_end = content_end;
        block.closed = closed;
        const std::size_t tags_before = event.tags.size();
        lex_controls(text, pos + 1, content_end, block_end, 0, event);
        block.tags.assign(event.tags.begin() + static_cast<std::ptrdiff_t>(tags_before),
                          event.tags.end());
        event.blocks.push_back(std::move(block));
        chunk_begin = block_end;
        pos = block_end;
    }
    if (chunk_begin < text.size()) {
        event.text.push_back({unescape_text(text, chunk_begin, text.size()),
                              chunk_begin, text.size()});
    }
    return event;
}

}  // namespace audit
