#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "subtitle_audit/ass_doc.hpp"
#include "subtitle_audit/style_chain.hpp"
#include "subtitle_audit/tag_effect.hpp"
#include "subtitle_audit/tag_lexer.hpp"

namespace audit {

struct Segment {
    int index = 0;
    std::size_t begin = 0;
    std::size_t end = 0;
    std::string text;
    std::string style;
    std::vector<std::string> chain;
    OverrideState overrides;
};

struct EventRuns {
    std::vector<Segment> segments;
    TagStats tags;
    std::vector<StyleChainError> errors;
    int r_switch = 0;
    int bare_r_reset = 0;
};

EventRuns split_event_runs(const AssDocument &doc, const ChainAnalysis &chains,
                           const Event &event, int64_t frame = -1);

}  // namespace audit
