#pragma once

#include <map>
#include <string>
#include <unordered_map>
#include <vector>

#include "subtitle_audit/ass_doc.hpp"

namespace audit {

struct StyleChainError {
    int line = 0;
    std::string message;
};

struct ResolvedStyle {
    std::string name;
    std::vector<std::string> chain;
    std::map<std::string, std::string> fields;
};

struct ChainAnalysis {
    std::unordered_map<std::string, ResolvedStyle> styles;
    std::unordered_map<std::string, std::string> parent_of;
    std::vector<StyleChainError> errors;
    int max_depth = 0;
    int cycles = 0;

    bool ok() const { return errors.empty(); }
};

std::string infer_parent(const AssDocument &doc, const Style &style);
ChainAnalysis analyze_style_chains(const AssDocument &doc);
bool resolve_style(const AssDocument &doc, const std::string &name,
                   ResolvedStyle &out, StyleChainError &error);
const std::string *resolved_field(const ResolvedStyle &style, const std::string &key);

}  // namespace audit
