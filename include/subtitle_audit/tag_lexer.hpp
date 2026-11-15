#pragma once

#include <cstddef>
#include <string>
#include <vector>

namespace audit {

struct TagToken {
    std::string name;
    std::string argument;
    std::string raw;
    std::size_t begin = 0;
    std::size_t end = 0;
    std::size_t block_end = 0;
    int t_depth = 0;
};

struct TextChunk {
    std::string visible;
    std::size_t begin = 0;
    std::size_t end = 0;
};

struct OverrideBlock {
    std::size_t begin = 0;
    std::size_t end = 0;
    std::size_t content_end = 0;
    bool closed = true;
    std::vector<TagToken> tags;
};

struct LexedEvent {
    std::vector<TagToken> tags;
    std::vector<TextChunk> text;
    std::vector<OverrideBlock> blocks;
    int unclosed_braces = 0;
    int max_t_depth = 0;
};

bool is_known_tag(const std::string &name);
LexedEvent lex_event_text(const std::string &text);

}  // namespace audit
