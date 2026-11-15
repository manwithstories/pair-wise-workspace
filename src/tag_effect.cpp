#include "subtitle_audit/tag_effect.hpp"

#include <algorithm>
#include <cctype>
#include <cmath>
#include <sstream>

namespace audit {
namespace {

std::string trim_arg(const std::string &value) {
    std::string result = trim(value);
    if (!result.empty() && result.front() == '(') result.erase(result.begin());
    if (!result.empty() && result.back() == ')') result.pop_back();
    return trim(result);
}

bool parse_double(const std::string &text, double &value) {
    std::string clean = trim_arg(text);
    if (clean.empty()) return false;
    try {
        std::size_t used = 0;
        value = std::stod(clean, &used);
        return used > 0;
    } catch (...) {
        return false;
    }
}

bool parse_int(const std::string &text, int &value) {
    std::string clean = trim_arg(text);
    if (clean.empty()) return false;
    try {
        std::size_t used = 0;
        value = std::stoi(clean, &used);
        return used > 0;
    } catch (...) {
        return false;
    }
}

std::vector<std::string> comma_parts(const std::string &argument) {
    std::vector<std::string> parts;
    std::stringstream stream(trim_arg(argument));
    std::string part;
    while (std::getline(stream, part, ',')) parts.push_back(trim(part));
    return parts;
}

double base_number(const OverrideState &state, const std::string &field,
                   const std::string &style, const TagEvalContext &ctx,
                   double fallback) {
    const std::optional<double> &override = field == "fs" ? state.font_size : state.border;
    if (override) return *override;
    if (!ctx.chains) return fallback;
    const auto it = ctx.chains->styles.find(style);
    if (it == ctx.chains->styles.end()) return fallback;
    const std::string *value = resolved_field(it->second, field == "fs" ? "fontsize" : "outline");
    double parsed = fallback;
    if (value && parse_double(*value, parsed)) return parsed;
    return fallback;
}

void blend_number(std::optional<double> &value, double before, double target, double progress) {
    value = before + (target - before) * progress;
}

void interpolate_state(OverrideState &state, const OverrideState &before,
                       const OverrideState &target, double progress,
                       const std::string &style, const TagEvalContext &ctx) {
    if (target.font_size) {
        blend_number(state.font_size,
                     base_number(before, "fs", style, ctx, *target.font_size),
                     *target.font_size, progress);
    }
    if (target.border) {
        blend_number(state.border,
                     base_number(before, "bord", style, ctx, *target.border),
                     *target.border, progress);
    }
    if (target.position) {
        if (before.position) {
            state.position = Point{
                before.position->x + (target.position->x - before.position->x) * progress,
                before.position->y + (target.position->y - before.position->y) * progress};
        } else {
            state.position = target.position;
        }
    }
    if (target.alignment) state.alignment = target.alignment;
    if (target.fade) state.fade = target.fade;
}

std::size_t apply_range(const std::vector<TagToken> &tokens, std::size_t index, int depth,
                        const TagEvalContext &ctx, OverrideState &state,
                        std::string &active_style, const std::string &reset_style,
                        std::vector<StyleChainError> &errors);

bool transform_active(const TagToken &tag, const TagEvalContext &ctx, double &progress) {
    const auto parts = comma_parts(tag.argument);
    int start = 0, end = 0;
    if (parts.size() >= 2) {
        parse_int(parts[0], start);
        parse_int(parts[1], end);
    } else {
        end = static_cast<int>(ctx.event_end - ctx.event_start);
    }
    const int64_t abs_start = ctx.event_start + start;
    const int64_t abs_end = ctx.event_start + end;
    if (ctx.frame < abs_start || ctx.frame > abs_end || abs_end <= abs_start) return false;
    progress = std::clamp(
        static_cast<double>(ctx.frame - abs_start) / (abs_end - abs_start), 0.0, 1.0);
    return true;
}

std::size_t apply_range(const std::vector<TagToken> &tokens, std::size_t index, int depth,
                        const TagEvalContext &ctx, OverrideState &state,
                        std::string &active_style, const std::string &reset_style,
                        std::vector<StyleChainError> &errors) {
    while (index < tokens.size()) {
        const TagToken &tag = tokens[index];
        if (tag.t_depth < depth) return index;
        if (tag.t_depth > depth) { ++index; continue; }
        if (tag.name == "r") {
            const std::string requested = trim(tag.argument);
            active_style = requested.empty() ? reset_style : requested;
            state = OverrideState{};
        } else if (tag.name == "fs") {
            double value = 0;
            if (parse_double(tag.argument, value)) state.font_size = value;
        } else if (tag.name == "bord") {
            double value = 0;
            if (parse_double(tag.argument, value)) state.border = value;
        } else if (tag.name == "an") {
            int value = 0;
            if (parse_int(tag.argument, value)) state.alignment = value;
        } else if (tag.name == "pos") {
            const auto parts = comma_parts(tag.argument);
            if (parts.size() >= 2) {
                Point point;
                if (parse_double(parts[0], point.x) && parse_double(parts[1], point.y))
                    state.position = point;
            }
        } else if (tag.name == "fad" || tag.name == "fade") {
            const auto parts = comma_parts(tag.argument);
            if (parts.size() >= 2) {
                Fade fade;
                if (parse_int(parts[0], fade.in_ms) && parse_int(parts[1], fade.out_ms))
                    state.fade = fade;
            }
        } else if (tag.name == "t") {
            std::size_t next = index + 1;
            while (next < tokens.size() && tokens[next].t_depth > tag.t_depth) ++next;
            double progress = 1.0;
            if (transform_active(tag, ctx, progress)) {
                const OverrideState before = state;
                OverrideState target;
                std::string nested_style = active_style;
                apply_range(tokens, index + 1, tag.t_depth + 1, ctx, target, nested_style,
                            reset_style, errors);
                interpolate_state(state, before, target, progress, active_style, ctx);
            }
            index = next;
            continue;
        }
        ++index;
    }
    return index;
}

}  // namespace

TagStats summarize_tags(const LexedEvent &event) {
    TagStats stats;
    stats.total = static_cast<int>(event.tags.size());
    stats.unclosed_braces = event.unclosed_braces;
    stats.max_t_depth = event.max_t_depth;
    for (const TagToken &tag : event.tags) {
        stats.counts[tag.name] += 1;
        if (!is_known_tag(tag.name)) ++stats.unknown;
        if (tag.t_depth > 0) ++stats.t_nested;
        if (tag.name == "r") {
            if (trim(tag.argument).empty()) ++stats.bare_r_reset;
            else ++stats.r_switch;
        }
    }
    return stats;
}

void apply_override_block(const std::vector<TagToken> &tokens, const TagEvalContext &ctx,
                          const std::string &event_style, std::string &active_style,
                          OverrideState &state, std::vector<StyleChainError> &errors) {
    (void)apply_range(tokens, 0, 0, ctx, state, active_style, event_style, errors);
}

}  // namespace audit
