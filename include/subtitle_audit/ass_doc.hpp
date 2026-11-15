#pragma once

#include <cstdint>
#include <map>
#include <optional>
#include <string>
#include <unordered_map>
#include <vector>

namespace audit {

struct Diagnostic {
    int line = 0;
    std::string message;
};

struct Style {
    std::string name;
    int line = 0;
    std::map<std::string, std::string> fields;
    std::optional<std::string> parent;
};

struct Event {
    int line = 0;
    int layer = 0;
    std::string start;
    std::string end;
    int64_t start_ms = 0;
    int64_t end_ms = 0;
    std::string style;
    std::string name;
    std::string effect;
    std::string text;
};

struct AssDocument {
    std::map<std::string, std::string> info;
    std::vector<Style> styles;
    std::unordered_map<std::string, std::size_t> style_index;
    std::vector<Event> events;
    std::vector<Diagnostic> diagnostics;
    std::string source_path;
    std::string parse_error;

    bool valid() const { return parse_error.empty(); }
    const Style *find_style(const std::string &name) const;
};

std::string trim(std::string value);
std::string lower_ascii(std::string value);
const std::string *style_field(const Style &style, const std::string &key);
bool parse_ass_time(const std::string &text, int64_t &out_ms);
AssDocument parse_ass_memory(const std::string &data, const std::string &source_path);
AssDocument parse_ass_file(const std::string &path);

}  // namespace audit
