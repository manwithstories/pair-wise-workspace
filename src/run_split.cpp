#include "subtitle_audit/run_split.hpp"

#include <algorithm>
#include <set>

namespace audit {
namespace {

std::string visible_text(const LexedEvent &lexed, std::size_t begin, std::size_t end) {
    std::string result;
    for (const TextChunk &chunk : lexed.text) {
        if (chunk.end <= begin || chunk.begin >= end) continue;
        result += chunk.visible;
    }
    return result;
}

void add_error(std::vector<StyleChainError> &errors, std::set<std::string> &seen,
               const StyleChainError &error) {
    const std::string key = std::to_string(error.line) + ":" + error.message;
    if (seen.insert(key).second) errors.push_back(error);
}

bool chain_for(const AssDocument &doc, const ChainAnalysis &chains,
               const std::string &style, std::vector<std::string> &chain,
               std::vector<StyleChainError> &errors, std::set<std::string> &seen) {
    const auto found = chains.styles.find(style);
    if (found != chains.styles.end()) {
        chain = found->second.chain;
        return true;
    }
    ResolvedStyle resolved;
    StyleChainError error;
    if (!resolve_style(doc, style, resolved, error)) {
        add_error(errors, seen, error);
        chain.clear();
        return false;
    }
    chain = resolved.chain;
    return true;
}

}  // namespace

EventRuns split_event_runs(const AssDocument &doc, const ChainAnalysis &chains,
                           const Event &event, int64_t frame) {
    EventRuns runs;
    const LexedEvent lexed = lex_event_text(event.text);
    runs.tags = summarize_tags(lexed);
    runs.r_switch = runs.tags.r_switch;
    runs.bare_r_reset = runs.tags.bare_r_reset;
    if (frame < 0) frame = event.start_ms + (event.end_ms - event.start_ms) / 2;
    TagEvalContext ctx{&doc, &chains, event.start_ms, event.end_ms, frame};

    std::string active_style = event.style;
    OverrideState state;
    std::size_t segment_begin = 0;
    std::string segment_text;
    std::set<std::string> error_keys;

    const auto close_segment = [&](std::size_t end) {
        if (segment_text.empty()) {
            segment_begin = end;
            return;
        }
        Segment segment;
        segment.index = static_cast<int>(runs.segments.size());
        segment.begin = segment_begin;
        segment.end = end;
        segment.text = segment_text;
        segment.style = active_style;
        segment.overrides = state;
        chain_for(doc, chains, active_style, segment.chain, runs.errors, error_keys);
        runs.segments.push_back(std::move(segment));
        segment_text.clear();
        segment_begin = end;
    };

    for (const OverrideBlock &block : lexed.blocks) {
        segment_text += visible_text(lexed, segment_begin, block.begin);
        close_segment(block.begin);
        apply_override_block(block.tags, ctx, event.style, active_style, state, runs.errors);
        std::vector<std::string> active_chain;
        if (!chain_for(doc, chains, active_style, active_chain, runs.errors, error_keys)) {
            // The empty chain is only used for diagnostics; text still gets a segment.
        }
        segment_begin = block.end;
    }
    segment_text += visible_text(lexed, segment_begin, event.text.size());
    close_segment(event.text.size());

    for (const OverrideBlock &block : lexed.blocks) {
        if (!block.closed) {
            add_error(runs.errors, error_keys, {event.line, "unclosed override brace"});
        }
    }
    return runs;
}

}  // namespace audit
