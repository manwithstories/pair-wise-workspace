#include "subtitle_audit/ass_doc.hpp"

#include <algorithm>
#include <cctype>
#include <fstream>
#include <sstream>

namespace audit {
namespace {

std::vector<std::string> split_lines(const std::string &data) {
    std::vector<std::string> lines;
    std::string current;
    for (std::size_t i = 0; i < data.size(); ++i) {
        const char ch = data[i];
        if (ch == '\r' || ch == '\n') {
            lines.push_back(current);
            current.clear();
            if (ch == '\r' && i + 1 < data.size() && data[i + 1] == '\n') ++i;
        } else {
            current.push_back(ch);
        }
    }
    if (!current.empty() || (!data.empty() && data.back() != '\n' && data.back() != '\r')) {
        lines.push_back(current);
    }
    return lines;
}
std::vector<std::string> split_simple(const std::string &line, std::size_t count,
                                      bool &ok) {
    std::vector<std::string> parts(count);
    std::size_t begin = 0;
    ok = true;
    if (count == 0) return parts;
    for (std::size_t i = 0; i < count; ++i) {
        if (i + 1 == count) {
            parts[i] = line.substr(begin);
            break;
        }
        const auto comma = line.find(',', begin);
        if (comma == std::string::npos) {
            ok = false;
            parts[i] = line.substr(begin);
            break;
        }
        parts[i] = line.substr(begin, comma - begin);
        begin = comma + 1;
    }
    return parts;
}
std::vector<std::string> split_event(const std::string &line, std::size_t count,
                                     std::size_t text_index, bool &ok) {
    std::vector<std::string> parts(count);
    ok = true;
    std::size_t begin = 0;
    for (std::size_t i = 0; i < text_index; ++i) {
        const auto comma = line.find(',', begin);
        if (comma == std::string::npos) { ok = false; return parts; }
        parts[i] = line.substr(begin, comma - begin);
        begin = comma + 1;
    }
    std::size_t end = line.size();
    for (std::size_t i = count; i-- > text_index + 1;) {
        if (end <= begin) { ok = false; return parts; }
        const auto comma = line.rfind(',', end - 1);
        if (comma == std::string::npos || comma < begin) { ok = false; return parts; }
        parts[i] = line.substr(comma + 1, end - comma - 1);
        end = comma;
    }
    parts[text_index] = line.substr(begin, end - begin);
    return parts;
}
bool parse_parent_directive(const std::string &line, std::string &child,
                            std::string &parent) {
    std::string body = trim(line.substr(1));
    const auto colon = body.find(':');
    if (colon == std::string::npos) return false;
    const std::string key = lower_ascii(trim(body.substr(0, colon)));
    if (key != "parent" && key != "inherit" && key != "base") return false;
    body = trim(body.substr(colon + 1));
    auto sep = body.find('=');
    if (sep == std::string::npos) sep = body.find("->");
    if (sep == std::string::npos) return false;
    const std::size_t width = body.compare(sep, 2, "->") == 0 ? 2 : 1;
    child = trim(body.substr(0, sep));
    parent = trim(body.substr(sep + width));
    return !child.empty() && !parent.empty();
}
void add_diag(AssDocument &doc, int line, std::string message) {
    doc.diagnostics.push_back({line, std::move(message)});
}
void set_error(AssDocument &doc, int line, const std::string &message) {
    if (doc.parse_error.empty()) {
        doc.parse_error = "line " + std::to_string(line) + ": " + message;
    }
}
}  // namespace
std::string trim(std::string value) {
    const auto not_space = [](unsigned char ch) { return !std::isspace(ch); };
    value.erase(value.begin(), std::find_if(value.begin(), value.end(), not_space));
    value.erase(std::find_if(value.rbegin(), value.rend(), not_space).base(), value.end());
    return value;
}
std::string lower_ascii(std::string value) {
    for (char &ch : value)
        ch = static_cast<char>(std::tolower(static_cast<unsigned char>(ch)));
    return value;
}

const Style *AssDocument::find_style(const std::string &name) const {
    const auto it = style_index.find(name);
    return it == style_index.end() ? nullptr : &styles[it->second];
}

const std::string *style_field(const Style &style, const std::string &key) {
    const auto it = style.fields.find(lower_ascii(key));
    return it == style.fields.end() ? nullptr : &it->second;
}

bool parse_ass_time(const std::string &text, int64_t &out_ms) {
    int hours = 0, minutes = 0, seconds = 0, centis = 0;
    char tail = 0;
    if (std::sscanf(text.c_str(), "%d:%d:%d.%d%c", &hours, &minutes, &seconds,
                    &centis, &tail) != 4)
        return false;
    if (hours < 0 || minutes < 0 || minutes > 59 || seconds < 0 || seconds > 59 ||
        centis < 0 || centis > 99)
        return false;
    out_ms = ((static_cast<int64_t>(hours) * 60 + minutes) * 60 + seconds) * 1000 +
             centis * 10;
    return true;
}

AssDocument parse_ass_memory(const std::string &input, const std::string &source_path) {
    AssDocument doc;
    doc.source_path = source_path;
    std::string data = input;
    if (data.size() >= 3 && static_cast<unsigned char>(data[0]) == 0xEF &&
        static_cast<unsigned char>(data[1]) == 0xBB &&
        static_cast<unsigned char>(data[2]) == 0xBF)
        data.erase(0, 3);

    enum class Section { None, Script, Styles, Events };
    Section section = Section::None;
    std::vector<std::string> style_format;
    std::vector<std::string> event_format;
    std::map<std::string, std::string> inline_parents;
    bool saw_events = false;
    const auto lines = split_lines(data);

    for (std::size_t index = 0; index < lines.size(); ++index) {
        const int line_no = static_cast<int>(index) + 1;
        const std::string raw = trim(lines[index]);
        if (raw.empty()) continue;
        if (raw.front() == ';') {
            std::string child, parent;
            if (parse_parent_directive(raw, child, parent))
                inline_parents[lower_ascii(child)] = parent;
            continue;
        }
        if (raw.front() == '[' && raw.back() == ']') {
            const std::string name = lower_ascii(trim(raw.substr(1, raw.size() - 2)));
            if (name == "script info") section = Section::Script;
            else if (name == "v4+ styles" || name == "v4 styles") section = Section::Styles;
            else if (name == "events") { section = Section::Events; saw_events = true; }
            else section = Section::None;
            continue;
        }
        if (section == Section::None) continue;
        const auto colon = raw.find(':');
        if (colon == std::string::npos) {
            set_error(doc, line_no, "expected 'key: value'");
            continue;
        }
        const std::string key = lower_ascii(trim(raw.substr(0, colon)));
        const std::string value = trim(raw.substr(colon + 1));

        if (section == Section::Script) {
            doc.info[key] = value;
        } else if (section == Section::Styles) {
            if (key == "format") {
                style_format.clear();
                std::stringstream stream(value);
                std::string field;
                while (std::getline(stream, field, ','))
                    style_format.push_back(lower_ascii(trim(field)));
                if (style_format.empty()) set_error(doc, line_no, "empty style Format");
            } else if (key == "style") {
                if (style_format.empty()) {
                    set_error(doc, line_no, "Style before Format");
                    continue;
                }
                bool ok = false;
                const auto values = split_simple(value, style_format.size(), ok);
                if (!ok || values.size() != style_format.size()) {
                    set_error(doc, line_no, "Style field count does not match Format");
                    continue;
                }
                Style style;
                style.line = line_no;
                for (std::size_t i = 0; i < values.size(); ++i)
                    style.fields[style_format[i]] = trim(values[i]);
                const auto name_it = style.fields.find("name");
                if (name_it == style.fields.end() || name_it->second.empty()) {
                    set_error(doc, line_no, "Style has no Name");
                    continue;
                }
                style.name = name_it->second;
                for (const char *parent_key : {"parent", "inherit", "base"}) {
                    const auto it = style.fields.find(parent_key);
                    if (it != style.fields.end() && !it->second.empty() && it->second != "*") {
                        style.parent = it->second;
                        break;
                    }
                }
                const auto duplicate = doc.style_index.find(style.name);
                if (duplicate != doc.style_index.end()) {
                    add_diag(doc, line_no, "duplicate style '" + style.name +
                        "' (first defined at line " +
                        std::to_string(doc.styles[duplicate->second].line) + ")");
                } else {
                    doc.style_index[style.name] = doc.styles.size();
                }
                doc.styles.push_back(std::move(style));
            } else {
                add_diag(doc, line_no, "unknown style section key '" + key + "'");
            }
        } else {
            if (key == "format") {
                event_format.clear();
                std::stringstream stream(value);
                std::string field;
                while (std::getline(stream, field, ','))
                    event_format.push_back(lower_ascii(trim(field)));
                if (event_format.empty()) set_error(doc, line_no, "empty event Format");
                continue;
            }
            if (key != "dialogue" && key != "comment") continue;
            if (event_format.empty()) {
                set_error(doc, line_no, "Dialogue before Format");
                continue;
            }
            std::size_t text_index = std::string::npos;
            for (std::size_t i = 0; i < event_format.size(); ++i)
                if (event_format[i] == "text") text_index = i;
            if (text_index == std::string::npos) {
                set_error(doc, line_no, "event Format has no Text field");
                continue;
            }
            bool ok = false;
            const auto values = split_event(value, event_format.size(), text_index, ok);
            if (!ok) {
                set_error(doc, line_no, "Dialogue field count does not match Format");
                continue;
            }
            std::map<std::string, std::string> fields;
            for (std::size_t i = 0; i < values.size(); ++i) fields[event_format[i]] = values[i];
            Event event;
            event.line = line_no;
            event.text = fields["text"];
            event.style = trim(fields["style"]);
            event.name = trim(fields["name"]);
            event.effect = trim(fields["effect"]);
            event.start = trim(fields["start"]);
            event.end = trim(fields["end"]);
            try {
                if (!fields["layer"].empty()) event.layer = std::stoi(trim(fields["layer"]));
            } catch (...) { event.layer = 0; }
            if (!parse_ass_time(event.start, event.start_ms) ||
                !parse_ass_time(event.end, event.end_ms) || event.end_ms < event.start_ms) {
                set_error(doc, line_no, "invalid event time range");
                continue;
            }
            if (key == "dialogue") doc.events.push_back(std::move(event));
        }
    }
    for (Style &style : doc.styles) {
        if (style.parent) continue;
        const auto it = inline_parents.find(lower_ascii(style.name));
        if (it != inline_parents.end()) style.parent = it->second;
    }
    if (!saw_events) set_error(doc, lines.empty() ? 1 : static_cast<int>(lines.size()),
                               "missing [Events] section");
    return doc;
}

AssDocument parse_ass_file(const std::string &path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        AssDocument doc;
        doc.source_path = path;
        doc.parse_error = "cannot open script";
        return doc;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    return parse_ass_memory(buffer.str(), path);
}

}  // namespace audit
