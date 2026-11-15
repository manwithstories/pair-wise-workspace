#include "subtitle_audit/style_chain.hpp"

#include <algorithm>
#include <cctype>
#include <set>

namespace audit {
namespace {

bool ends_with(const std::string &value, const std::string &suffix) {
    return value.size() >= suffix.size() &&
           value.compare(value.size() - suffix.size(), suffix.size(), suffix) == 0;
}

bool starts_with(const std::string &value, const std::string &prefix) {
    return value.compare(0, prefix.size(), prefix) == 0;
}

bool looks_like_episode(const std::string &name) {
    if (name.size() < 4 || name[0] != 'e' || name[1] != 'p') return false;
    return std::isdigit(static_cast<unsigned char>(name[2])) != 0;
}

std::string strip_suffix(const std::string &name, const std::string &suffix) {
    return name.substr(0, name.size() - suffix.size());
}

bool is_language_tag(const std::string &tag) {
    if (tag.size() < 2 || tag.size() > 3) return false;
    return std::all_of(tag.begin(), tag.end(), [](unsigned char ch) {
        return std::islower(ch) != 0;
    });
}

std::string longest_prefix_parent(const AssDocument &doc, const std::string &name) {
    std::string best;
    for (const Style &candidate : doc.styles) {
        if (candidate.name.size() >= name.size() || candidate.name == name) continue;
        if (name.compare(0, candidate.name.size(), candidate.name) != 0) continue;
        if (name[candidate.name.size()] != '_') continue;
        if (candidate.name.size() > best.size()) best = candidate.name;
    }
    return best;
}

}  // namespace

std::string infer_parent(const AssDocument &doc, const Style &style) {
    if (style.parent) return *style.parent;
    if (starts_with(style.name, "template")) return "";
    std::string candidate;
    if (ends_with(style.name, "_title")) {
        candidate = strip_suffix(style.name, "_title");
        if (!doc.find_style(candidate)) candidate.clear();
    }
    if (candidate.empty()) {
        const auto pos = style.name.rfind('_');
        if (pos != std::string::npos && is_language_tag(style.name.substr(pos + 1))) {
            candidate = style.name.substr(0, pos);
            if (!doc.find_style(candidate)) candidate.clear();
        }
    }
    if (candidate.empty() && looks_like_episode(style.name)) {
        if (doc.find_style("season1")) candidate = "season1";
    }
    if (candidate.empty()) candidate = longest_prefix_parent(doc, style.name);
    if (candidate.empty() && style.name == "season1" && doc.find_style("template"))
        candidate = "template";
    return candidate;
}

std::string canonical_name(const AssDocument &doc, const std::string &requested) {
    if (doc.find_style(requested)) return requested;
    for (const Style &style : doc.styles)
        if (lower_ascii(style.name) == lower_ascii(requested)) return style.name;
    return requested;
}

bool build_chain(const AssDocument &doc, const std::string &requested,
                 ResolvedStyle &out, StyleChainError &error) {
    const std::string name = canonical_name(doc, requested);
    const Style *style = doc.find_style(name);
    if (!style) {
        error = {0, "event references missing style '" + requested + "'"};
        return false;
    }
    std::vector<const Style *> upward;
    std::set<std::string> seen;
    const Style *current = style;
    while (current) {
        if (!seen.insert(current->name).second) {
            error = {current->line, "style inheritance cycle at '" + current->name + "'"};
            return false;
        }
        upward.push_back(current);
        const std::string parent = infer_parent(doc, *current);
        if (parent.empty()) break;
        const std::string canonical_parent = canonical_name(doc, parent);
        const Style *next = doc.find_style(canonical_parent);
        if (!next) {
            error = {current->line, "style '" + current->name +
                     "' references missing parent '" + parent + "'"};
            return false;
        }
        current = next;
    }
    out = ResolvedStyle{};
    out.name = style->name;
    for (auto it = upward.rbegin(); it != upward.rend(); ++it) {
        out.chain.push_back((*it)->name);
        for (const auto &[key, value] : (*it)->fields) {
            if (key == "parent" || key == "inherit" || key == "base") continue;
            if (value.empty() || value == "*") continue;
            out.fields[key] = value;
        }
    }
    out.fields["name"] = style->name;
    return true;
}

ChainAnalysis analyze_style_chains(const AssDocument &doc) {
    ChainAnalysis analysis;
    std::map<std::string, const Style *> by_name;
    for (const Style &style : doc.styles) {
        by_name.emplace(style.name, &style);
        analysis.parent_of[style.name] = infer_parent(doc, style);
    }
    for (const Style &style : doc.styles) {
        ResolvedStyle resolved;
        StyleChainError error;
        if (!build_chain(doc, style.name, resolved, error)) {
            analysis.errors.push_back(error);
            continue;
        }
        analysis.max_depth = std::max(analysis.max_depth,
                                      static_cast<int>(resolved.chain.size()));
        analysis.styles.emplace(style.name, std::move(resolved));
    }
    std::set<std::string> visited;
    for (const Style &style : doc.styles) {
        std::vector<std::string> path;
        std::map<std::string, std::size_t> position;
        std::string current = style.name;
        while (!current.empty() && !visited.count(current)) {
            const auto found = position.find(current);
            if (found != position.end()) {
                ++analysis.cycles;
                const Style *cycle_style = doc.find_style(current);
                analysis.errors.push_back({cycle_style ? cycle_style->line : style.line,
                    "style inheritance cycle at '" + current + "'"});
                break;
            }
            position[current] = path.size();
            path.push_back(current);
            const auto parent = analysis.parent_of.find(current);
            if (parent == analysis.parent_of.end() || parent->second.empty()) break;
            if (!doc.find_style(parent->second)) break;
            current = canonical_name(doc, parent->second);
        }
        for (const std::string &name : path) visited.insert(name);
    }
    std::vector<StyleChainError> unique;
    std::set<std::pair<int, std::string>> seen_errors;
    for (const StyleChainError &error : analysis.errors) {
        if (seen_errors.insert({error.line, error.message}).second)
            unique.push_back(error);
    }
    analysis.errors = std::move(unique);
    return analysis;
}

bool resolve_style(const AssDocument &doc, const std::string &name,
                   ResolvedStyle &out, StyleChainError &error) {
    return build_chain(doc, name, out, error);
}

const std::string *resolved_field(const ResolvedStyle &style, const std::string &key) {
    const auto it = style.fields.find(lower_ascii(key));
    return it == style.fields.end() ? nullptr : &it->second;
}

}  // namespace audit
