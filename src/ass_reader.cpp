#include "subqc/ass_reader.hpp"

#include <algorithm>
#include <cctype>
#include <fstream>
#include <sstream>

namespace subqc {
namespace {

enum class Section { None, ScriptInfo, Styles, Events };

std::vector<std::string> split_csv(const std::string &line, bool lower_names) {
    std::vector<std::string> fields;
    std::stringstream stream(line);
    std::string field;
    while (std::getline(stream, field, ',')) {
        field = lower_names ? lower_copy(trim_copy(field)) : trim_copy(field);
        fields.push_back(field);
    }
    return fields;
}

double to_number(const std::string &value, double fallback = 0.0) {
    try {
        std::size_t used = 0;
        double result = std::stod(trim_copy(value), &used);
        return used == 0 ? fallback : result;
    } catch (...) {
        return fallback;
    }
}

std::string utf16_to_utf8(const std::string &bytes, bool little_endian) {
    std::string out;
    for (std::size_t i = 2; i + 1 < bytes.size(); i += 2) {
        const auto byte0 = static_cast<unsigned char>(bytes[i]);
        const auto byte1 = static_cast<unsigned char>(bytes[i + 1]);
        std::uint32_t codepoint = little_endian ? (byte1 << 8) | byte0 : (byte0 << 8) | byte1;
        if (codepoint >= 0xD800 && codepoint <= 0xDBFF && i + 3 < bytes.size()) {
            const auto next0 = static_cast<unsigned char>(bytes[i + 2]);
            const auto next1 = static_cast<unsigned char>(bytes[i + 3]);
            const std::uint32_t low = little_endian ? (next1 << 8) | next0 : (next0 << 8) | next1;
            if (low >= 0xDC00 && low <= 0xDFFF) {
                codepoint = 0x10000 + ((codepoint - 0xD800) << 10) + (low - 0xDC00);
                i += 2;
            } else {
                codepoint = 0xFFFD;
            }
        } else if (codepoint >= 0xD800 && codepoint <= 0xDFFF) {
            codepoint = 0xFFFD;
        }
        if (codepoint < 0x80) {
            out.push_back(static_cast<char>(codepoint));
        } else if (codepoint < 0x800) {
            out.push_back(static_cast<char>(0xC0 | (codepoint >> 6)));
            out.push_back(static_cast<char>(0x80 | (codepoint & 0x3F)));
        } else if (codepoint < 0x10000) {
            out.push_back(static_cast<char>(0xE0 | (codepoint >> 12)));
            out.push_back(static_cast<char>(0x80 | ((codepoint >> 6) & 0x3F)));
            out.push_back(static_cast<char>(0x80 | (codepoint & 0x3F)));
        } else {
            out.push_back(static_cast<char>(0xF0 | (codepoint >> 18)));
            out.push_back(static_cast<char>(0x80 | ((codepoint >> 12) & 0x3F)));
            out.push_back(static_cast<char>(0x80 | ((codepoint >> 6) & 0x3F)));
            out.push_back(static_cast<char>(0x80 | (codepoint & 0x3F)));
        }
    }
    return out;
}

std::int64_t to_integer(const std::string &value, std::int64_t fallback = 0) {
    try {
        std::size_t used = 0;
        std::int64_t result = std::stoll(trim_copy(value), &used, 10);
        return used == 0 ? fallback : result;
    } catch (...) {
        return fallback;
    }
}

Section parse_section(const std::string &line) {
    std::string value = lower_copy(trim_copy(line));
    if (value == "[script info]") return Section::ScriptInfo;
    if (value == "[v4+ styles]" || value == "[v4 styles]" || value == "[v4++ styles]")
        return Section::Styles;
    if (value == "[events]") return Section::Events;
    return Section::None;
}

bool is_dialogue(const std::string &line) {
    const std::string lower = lower_copy(line);
    return lower.size() >= 9 && lower.compare(0, 9, "dialogue:") == 0;
}

void assign_style(std::vector<std::string> &format, const std::string &rest,
                  RawStyle &style, int line_no, std::vector<Diagnostic> &diagnostics) {
    auto values = split_ass_fields(rest, format.size());
    if (values.size() < format.size()) {
        diagnostics.push_back({"warning", "", line_no, "style_short", "Style row has fewer fields than Format"});
        values.resize(format.size());
    }
    for (std::size_t i = 0; i < format.size(); ++i) {
        if (format[i].empty()) continue;
        style.fields[format[i]] = values[i];
        if (format[i] == "name") style.name = values[i];
    }
    style.line = line_no;
}

void assign_event(std::vector<std::string> &format, const std::string &rest,
                  Event &event, int line_no, int input_index,
                  std::vector<Diagnostic> &diagnostics) {
    auto values = split_ass_fields(rest, format.size());
    if (values.size() < format.size()) {
        diagnostics.push_back({"warning", "", line_no, "event_short", "Dialogue row has fewer fields than Format"});
        values.resize(format.size());
    }
    event.line = line_no;
    event.input_index = input_index;
    for (std::size_t i = 0; i < format.size(); ++i) {
        const std::string &name = format[i];
        const std::string &value = values[i];
        if (name == "layer") event.layer = static_cast<int>(to_integer(value));
        else if (name == "start") { event.start_text = value; event.start_ms = parse_ass_time(value); }
        else if (name == "end") { event.end_text = value; event.end_ms = parse_ass_time(value); }
        else if (name == "style") event.style = value;
        else if (name == "name") event.name = value;
        else if (name == "marginl") event.margin_l = static_cast<int>(to_integer(value));
        else if (name == "marginr") event.margin_r = static_cast<int>(to_integer(value));
        else if (name == "marginv") event.margin_v = static_cast<int>(to_integer(value));
        else if (name == "effect") event.effect = value;
        else if (name == "text") event.text = value;
    }
    if (event.end_ms < event.start_ms) {
        std::swap(event.start_ms, event.end_ms);
        std::swap(event.start_text, event.end_text);
        diagnostics.push_back({"warning", "", line_no, "event_time", "Dialogue end precedes start; swapped"});
    }
}

void read_script_info(const std::string &line, AssFile &file) {
    const auto colon = line.find(':');
    if (colon == std::string::npos) return;
    const std::string key = lower_copy(trim_copy(line.substr(0, colon)));
    const std::string value = trim_copy(line.substr(colon + 1));
    if (key == "playresx") {
        file.info.play_res_x = std::max(1, static_cast<int>(to_integer(value, 384)));
        file.info.play_res_missing = false;
    } else if (key == "playresy") {
        file.info.play_res_y = std::max(1, static_cast<int>(to_integer(value, 288)));
        file.info.play_res_missing = false;
    } else if (key == "wrapstyle") {
        file.info.wrap_style = value;
    } else if (key == "scaledborderandshadow") {
        file.info.scaled_border = lower_copy(value);
    }
}

}  // namespace

std::string trim_copy(const std::string &value) {
    const auto begin = value.find_first_not_of(" \t\r\n");
    if (begin == std::string::npos) return {};
    const auto end = value.find_last_not_of(" \t\r\n");
    return value.substr(begin, end - begin + 1);
}

std::string lower_copy(std::string value) {
    std::transform(value.begin(), value.end(), value.begin(),
                   [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
    return value;
}

double parse_ass_time(const std::string &value) {
    std::vector<std::string> parts;
    std::stringstream stream(trim_copy(value));
    std::string part;
    while (std::getline(stream, part, ':')) parts.push_back(trim_copy(part));
    if (parts.size() != 3) return 0.0;
    const auto dot = parts[2].find('.');
    const std::string seconds_text = dot == std::string::npos ? parts[2] : parts[2].substr(0, dot);
    const std::string fraction_text = dot == std::string::npos ? "0" : parts[2].substr(dot + 1);
    double fraction = to_number("0." + fraction_text);
    return to_number(parts[0]) * 3600000.0 + to_number(parts[1]) * 60000.0
           + to_number(seconds_text) * 1000.0 + fraction * 1000.0;
}

std::vector<std::string> split_ass_fields(const std::string &line, std::size_t max_fields) {
    std::vector<std::string> fields;
    if (max_fields == 0) return fields;
    std::size_t start = 0;
    while (fields.size() + 1 < max_fields) {
        const auto comma = line.find(',', start);
        if (comma == std::string::npos) break;
        fields.push_back(trim_copy(line.substr(start, comma - start)));
        start = comma + 1;
    }
    fields.push_back(trim_copy(line.substr(start)));
    return fields;
}

std::vector<std::filesystem::path> collect_ass_files(const std::filesystem::path &input) {
    std::vector<std::filesystem::path> files;
    std::error_code ec;
    if (std::filesystem::is_regular_file(input, ec)) {
        files.push_back(input);
    } else if (std::filesystem::is_directory(input, ec)) {
        for (const auto &entry : std::filesystem::recursive_directory_iterator(input, ec)) {
            if (!entry.is_regular_file()) continue;
            const std::string suffix = lower_copy(entry.path().extension().string());
            if (suffix == ".ass" || suffix == ".ssa") files.push_back(entry.path());
        }
    }
    std::sort(files.begin(), files.end());
    return files;
}

bool read_ass_file(const std::filesystem::path &path, AssFile &out) {
    out = AssFile{};
    out.path = path.string();
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        out.fatal = true;
        out.diagnostics.push_back({"error", out.path, 0, "read_failed", "Cannot open ASS file"});
        return false;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    std::string bytes = buffer.str();
    if (bytes.size() >= 3 && static_cast<unsigned char>(bytes[0]) == 0xEF &&
        static_cast<unsigned char>(bytes[1]) == 0xBB &&
        static_cast<unsigned char>(bytes[2]) == 0xBF) {
        bytes.erase(0, 3);
    } else if (bytes.size() >= 2 && static_cast<unsigned char>(bytes[0]) == 0xFF &&
               static_cast<unsigned char>(bytes[1]) == 0xFE) {
        bytes = utf16_to_utf8(bytes, true);
    } else if (bytes.size() >= 2 && static_cast<unsigned char>(bytes[0]) == 0xFE &&
               static_cast<unsigned char>(bytes[1]) == 0xFF) {
        bytes = utf16_to_utf8(bytes, false);
    }

    Section section = Section::None;
    bool saw_section = false;
    std::vector<std::string> style_format;
    std::vector<std::string> event_format;
    int line_no = 0;
    std::string line;
    std::istringstream lines(bytes);
    while (std::getline(lines, line)) {
        ++line_no;
        if (!line.empty() && line.back() == '\r') line.pop_back();
        const std::string trimmed = trim_copy(line);
        if (trimmed.empty() || trimmed.front() == ';') continue;
        const Section header = parse_section(trimmed);
        if (header != Section::None) {
            section = header;
            saw_section = true;
            continue;
        }
        if (section == Section::ScriptInfo) {
            read_script_info(line, out);
            continue;
        }
        if (section == Section::Styles) {
            const std::string lower = lower_copy(trimmed);
            if (lower.rfind("format:", 0) == 0) {
                style_format = split_csv(trimmed.substr(trimmed.find(':') + 1), true);
                continue;
            }
            if (lower.rfind("style:", 0) == 0) {
                if (style_format.empty()) {
                    style_format = {"name", "fontname", "fontsize", "primarycolour", "secondarycolour",
                                    "outlinecolour", "backcolour", "bold", "italic", "underline",
                                    "strikeout", "scalex", "scaley", "spacing", "angle", "borderstyle",
                                    "outline", "shadow", "alignment", "marginl", "marginr", "marginv",
                                    "encoding"};
                }
                RawStyle style;
                assign_style(style_format, line.substr(line.find(':') + 1), style, line_no, out.diagnostics);
                if (!style.name.empty()) out.styles.push_back(std::move(style));
                continue;
            }
            continue;
        }
        if (section == Section::Events && is_dialogue(trimmed)) {
            if (event_format.empty()) {
                event_format = {"layer", "start", "end", "style", "name", "marginl", "marginr", "marginv",
                                "effect", "text"};
            }
            Event event;
            assign_event(event_format, trimmed.substr(trimmed.find(':') + 1), event, line_no,
                         static_cast<int>(out.events.size()), out.diagnostics);
            out.events.push_back(std::move(event));
        }
    }
    if (!saw_section) {
        out.fatal = true;
        out.diagnostics.push_back({"error", out.path, 0, "no_sections", "No ASS sections found"});
        return false;
    }
    if (out.info.play_res_missing) {
        out.diagnostics.push_back({"warning", out.path, 0, "playres_default",
                                   "PlayRes missing; using 384x288"});
    }
    for (auto &diagnostic : out.diagnostics) {
        if (diagnostic.file.empty()) diagnostic.file = out.path;
    }
    return true;
}

}  // namespace subqc
