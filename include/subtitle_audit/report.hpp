#pragma once

#include <string>
#include <utility>
#include <vector>

#include "subtitle_audit/ass_doc.hpp"
#include "subtitle_audit/layout_probe.hpp"
#include "subtitle_audit/style_chain.hpp"

namespace audit {

struct TagAggregate {
    int total = 0;
    int unknown = 0;
    int t_nested = 0;
    int r_switch = 0;
    int bare_r_reset = 0;
    int unclosed_braces = 0;
    int max_t_depth = 0;
    std::map<std::string, int> counts;

    std::vector<std::pair<std::string, int>> top(int limit) const;
};

struct BatchFileSummary {
    std::string path;
    std::size_t events = 0;
    std::size_t styles = 0;
    int tags = 0;
    int unknown = 0;
    int t_nested = 0;
    int r_switch = 0;
    int visible = 0;
    int max_width = 0;
    int max_height = 0;
    bool parse_ok = true;
    bool render_ok = true;
    std::string error;
    std::vector<std::string> missing_fonts;
};

TagAggregate aggregate_tags(const AssDocument &doc);
std::vector<std::string> deepest_chain(const ChainAnalysis &analysis);
std::string chain_report_json(const AssDocument &doc, const ChainAnalysis &analysis);
std::string tag_report_json(const AssDocument &doc, const TagAggregate &tags);
std::string probe_report_json(const AssDocument &doc, const ProbeResult &probe);
std::string batch_report_json(const std::string &directory,
                              const std::vector<BatchFileSummary> &files,
                              int64_t frame);
bool write_text_file(const std::string &path, const std::string &content);

}  // namespace audit
