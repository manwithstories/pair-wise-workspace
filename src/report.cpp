#include "subtitle_audit/report.hpp"

#include <algorithm>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <map>
#include <set>
#include <sstream>

#include "subtitle_audit/run_split.hpp"
#include "subtitle_audit/tag_lexer.hpp"

namespace audit {
namespace {

std::string escape(const std::string &value) {
    std::ostringstream out;
    for (unsigned char ch : value) {
        switch (ch) {
            case '"': out << "\\\""; break;
            case '\\': out << "\\\\"; break;
            case '\b': out << "\\b"; break;
            case '\f': out << "\\f"; break;
            case '\n': out << "\\n"; break;
            case '\r': out << "\\r"; break;
            case '\t': out << "\\t"; break;
            default:
                if (ch < 0x20) out << "\\u" << std::hex << std::setw(4)
                                   << std::setfill('0') << static_cast<int>(ch);
                else out << static_cast<char>(ch);
        }
    }
    return out.str();
}

std::string string_array(const std::vector<std::string> &values) {
    std::ostringstream out;
    out << '[';
    for (std::size_t i = 0; i < values.size(); ++i) {
        if (i) out << ',';
        out << '"' << escape(values[i]) << '"';
    }
    out << ']';
    return out.str();
}

void write_overrides(std::ostringstream &out, const OverrideState &state) {
    out << '{';
    bool comma = false;
    const auto separator = [&]() { if (comma) out << ','; comma = true; };
    if (state.font_size) { separator(); out << "\"font_size\":" << *state.font_size; }
    if (state.border) { separator(); out << "\"border\":" << *state.border; }
    if (state.alignment) { separator(); out << "\"alignment\":" << *state.alignment; }
    if (state.position) {
        separator();
        out << "\"pos\":{\"x\":" << state.position->x << ",\"y\":" << state.position->y << '}';
    }
    if (state.fade) {
        separator();
        out << "\"fade\":{\"in\":" << state.fade->in_ms << ",\"out\":"
            << state.fade->out_ms << '}';
    }
    out << '}';
}

std::vector<std::string> leaf_first(const std::vector<std::string> &chain) {
    return {chain.rbegin(), chain.rend()};
}

}  // namespace

std::vector<std::pair<std::string, int>> TagAggregate::top(int limit) const {
    std::vector<std::pair<std::string, int>> sorted(counts.begin(), counts.end());
    std::sort(sorted.begin(), sorted.end(), [](const auto &left, const auto &right) {
        if (left.second != right.second) return left.second > right.second;
        return left.first < right.first;
    });
    if (limit >= 0 && static_cast<std::size_t>(limit) < sorted.size())
        sorted.resize(static_cast<std::size_t>(limit));
    return sorted;
}

TagAggregate aggregate_tags(const AssDocument &doc) {
    TagAggregate aggregate;
    for (const Event &event : doc.events) {
        const TagStats stats = summarize_tags(lex_event_text(event.text));
        aggregate.total += stats.total;
        aggregate.unknown += stats.unknown;
        aggregate.t_nested += stats.t_nested;
        aggregate.r_switch += stats.r_switch;
        aggregate.bare_r_reset += stats.bare_r_reset;
        aggregate.unclosed_braces += stats.unclosed_braces;
        aggregate.max_t_depth = std::max(aggregate.max_t_depth, stats.max_t_depth);
        for (const auto &[name, count] : stats.counts) aggregate.counts[name] += count;
    }
    return aggregate;
}

std::vector<std::string> deepest_chain(const ChainAnalysis &analysis) {
    const ResolvedStyle *best = nullptr;
    for (const auto &[name, style] : analysis.styles) {
        (void)name;
        if (!best || style.chain.size() > best->chain.size() ||
            (style.chain.size() == best->chain.size() && style.name < best->name))
            best = &style;
    }
    return best ? leaf_first(best->chain) : std::vector<std::string>{};
}

std::string chain_report_json(const AssDocument &doc, const ChainAnalysis &analysis) {
    std::ostringstream out;
    out << "{\"script\":\"" << escape(doc.source_path) << "\",\"events\":"
        << doc.events.size() << ",\"styles\":" << doc.styles.size()
        << ",\"max_chain_depth\":" << analysis.max_depth << ",\"cycles\":"
        << analysis.cycles << ",\"chains\":{";
    bool comma = false;
    std::map<std::string, std::vector<std::string>> sorted_chains;
    for (const auto &[name, style] : analysis.styles)
        sorted_chains[name] = leaf_first(style.chain);
    for (const auto &[name, chain] : sorted_chains) {
        if (comma) out << ',';
        comma = true;
        out << '"' << escape(name) << "\":" << string_array(chain);
    }
    out << "},\"diagnostics\":[";
    for (std::size_t i = 0; i < doc.diagnostics.size(); ++i) {
        if (i) out << ',';
        out << "{\"line\":" << doc.diagnostics[i].line << ",\"message\":\""
            << escape(doc.diagnostics[i].message) << "\"}";
    }
    out << "],\"events_detail\":[";
    for (std::size_t i = 0; i < doc.events.size(); ++i) {
        if (i) out << ',';
        const Event &event = doc.events[i];
        const EventRuns runs = split_event_runs(doc, analysis, event);
        out << "{\"index\":" << i << ",\"line\":" << event.line << ",\"style\":\""
            << escape(event.style) << "\",\"segments\":[";
        for (std::size_t s = 0; s < runs.segments.size(); ++s) {
            if (s) out << ',';
            const Segment &segment = runs.segments[s];
            out << "{\"index\":" << segment.index << ",\"begin\":" << segment.begin
                << ",\"end\":" << segment.end << ",\"text\":\"" << escape(segment.text)
                << "\",\"style\":\"" << escape(segment.style) << "\",\"chain\":"
                << string_array(leaf_first(segment.chain)) << ",\"overrides\":";
            write_overrides(out, segment.overrides);
            out << '}';
        }
        out << "]}";
    }
    out << "]}";
    return out.str();
}

std::string tag_report_json(const AssDocument &doc, const TagAggregate &tags) {
    std::ostringstream out;
    out << "{\"script\":\"" << escape(doc.source_path) << "\",\"events\":"
        << doc.events.size() << ",\"tags\":" << tags.total << ",\"unknown\":"
        << tags.unknown << ",\"t_nested\":" << tags.t_nested << ",\"r_switch\":"
        << tags.r_switch << ",\"bare_r_reset\":" << tags.bare_r_reset
        << ",\"unclosed_braces\":" << tags.unclosed_braces << ",\"max_t_depth\":"
        << tags.max_t_depth << ",\"top\":[";
    const auto top = tags.top(10);
    for (std::size_t i = 0; i < top.size(); ++i) {
        if (i) out << ',';
        out << "{\"tag\":\"" << escape("\\" + top[i].first) << "\",\"count\":"
            << top[i].second << '}';
    }
    out << "],\"events_detail\":[";
    for (std::size_t i = 0; i < doc.events.size(); ++i) {
        if (i) out << ',';
        const TagStats stats = summarize_tags(lex_event_text(doc.events[i].text));
        out << "{\"index\":" << i << ",\"line\":" << doc.events[i].line
            << ",\"tags\":" << stats.total << ",\"unknown\":" << stats.unknown
            << ",\"t_nested\":" << stats.t_nested << ",\"r_switch\":"
            << stats.r_switch << ",\"unclosed_braces\":" << stats.unclosed_braces << '}';
    }
    out << "]}";
    return out.str();
}

std::string probe_report_json(const AssDocument &doc, const ProbeResult &probe) {
    std::ostringstream out;
    out << "{\"script\":\"" << escape(doc.source_path) << "\",\"frame\":" << probe.frame
        << ",\"width\":" << probe.width << ",\"height\":" << probe.height
        << ",\"visible\":" << probe.visible << ",\"max_width\":" << probe.max_width
        << ",\"max_height\":" << probe.max_height << ",\"missing_fonts\":"
        << string_array(probe.missing_fonts) << ",\"events\":[";
    for (std::size_t i = 0; i < probe.events.size(); ++i) {
        if (i) out << ',';
        const ProbeEvent &event = probe.events[i];
        out << "{\"index\":" << event.index << ",\"line\":" << event.line
            << ",\"visible\":" << (event.visible ? "true" : "false")
            << ",\"bbox\":{\"x\":" << event.bbox.x << ",\"y\":" << event.bbox.y
            << ",\"w\":" << event.bbox.w << ",\"h\":" << event.bbox.h
            << "},\"alpha_fnv1a\":\"" << escape(event.alpha_fnv1a)
            << "\",\"missing_fonts\":" << string_array(event.missing_fonts) << '}';
    }
    out << "]}";
    return out.str();
}

std::string batch_report_json(const std::string &directory,
                              const std::vector<BatchFileSummary> &files,
                              int64_t frame) {
    std::size_t events = 0, styles = 0;
    int tags = 0, visible = 0;
    std::set<std::string> missing;
    for (const BatchFileSummary &file : files) {
        events += file.events;
        styles += file.styles;
        tags += file.tags;
        visible += file.visible;
        missing.insert(file.missing_fonts.begin(), file.missing_fonts.end());
    }
    std::ostringstream out;
    out << "{\"dir\":\"" << escape(directory) << "\",\"frame\":" << frame
        << ",\"files\":" << files.size() << ",\"events\":" << events
        << ",\"styles\":" << styles << ",\"tags\":" << tags << ",\"visible\":"
        << visible << ",\"missing_fonts\":" << string_array({missing.begin(), missing.end()})
        << ",\"files_detail\":[";
    for (std::size_t i = 0; i < files.size(); ++i) {
        if (i) out << ',';
        const BatchFileSummary &file = files[i];
        out << "{\"file\":\"" << escape(file.path) << "\",\"events\":" << file.events
            << ",\"styles\":" << file.styles << ",\"tags\":" << file.tags
            << ",\"unknown\":" << file.unknown << ",\"t_nested\":" << file.t_nested
            << ",\"r_switch\":" << file.r_switch << ",\"visible\":" << file.visible
            << ",\"max_width\":" << file.max_width << ",\"max_height\":"
            << file.max_height << ",\"parse_ok\":" << (file.parse_ok ? "true" : "false")
            << ",\"render_ok\":" << (file.render_ok ? "true" : "false")
            << ",\"error\":\"" << escape(file.error) << "\",\"missing_fonts\":"
            << string_array(file.missing_fonts) << '}';
    }
    out << "]}";
    return out.str();
}

bool write_text_file(const std::string &path, const std::string &content) {
    std::error_code error;
    const std::filesystem::path output(path);
    if (output.has_parent_path())
        std::filesystem::create_directories(output.parent_path(), error);
    if (error) return false;
    std::ofstream file(path, std::ios::binary);
    if (!file) return false;
    file.write(content.data(), static_cast<std::streamsize>(content.size()));
    return static_cast<bool>(file);
}

}  // namespace audit
