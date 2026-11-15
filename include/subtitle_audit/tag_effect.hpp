#pragma once

#include <cstdint>
#include <map>
#include <optional>
#include <string>
#include <vector>

#include "subtitle_audit/ass_doc.hpp"
#include "subtitle_audit/style_chain.hpp"
#include "subtitle_audit/tag_lexer.hpp"

namespace audit {

struct Point {
    double x = 0;
    double y = 0;
};

struct Fade {
    int in_ms = 0;
    int out_ms = 0;
};

struct OverrideState {
    std::optional<double> font_size;
    std::optional<double> border;
    std::optional<int> alignment;
    std::optional<Point> position;
    std::optional<Fade> fade;

    bool empty() const {
        return !font_size && !border && !alignment && !position && !fade;
    }
};

struct TagStats {
    int total = 0;
    int unknown = 0;
    int t_nested = 0;
    int r_switch = 0;
    int bare_r_reset = 0;
    int unclosed_braces = 0;
    int max_t_depth = 0;
    std::map<std::string, int> counts;
};

struct TagEvalContext {
    const AssDocument *document = nullptr;
    const ChainAnalysis *chains = nullptr;
    int64_t event_start = 0;
    int64_t event_end = 0;
    int64_t frame = 0;
};

TagStats summarize_tags(const LexedEvent &event);
void apply_override_block(const std::vector<TagToken> &tokens, const TagEvalContext &ctx,
                          const std::string &event_style, std::string &active_style,
                          OverrideState &state, std::vector<StyleChainError> &errors);

}  // namespace audit
