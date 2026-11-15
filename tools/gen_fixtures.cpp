#include "gen_fixtures.hpp"

#include <algorithm>
#include <array>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <map>
#include <sstream>
#include <vector>

namespace audit {
namespace {

const std::vector<std::string> kStyleFields = {
    "name", "fontname", "fontsize", "primarycolour", "secondarycolour",
    "outlinecolour", "backcolour", "bold", "italic", "underline", "strikeout",
    "scalex", "scaley", "spacing", "angle", "borderstyle", "outline", "shadow",
    "alignment", "marginl", "marginr", "marginv", "encoding"};

const std::vector<std::string> kEventFields = {
    "layer", "start", "end", "style", "name", "marginl", "marginr", "marginv",
    "effect", "text"};

std::string field_header(const std::string &prefix,
                         const std::vector<std::string> &order) {
    std::ostringstream out;
    out << prefix << ": ";
    for (std::size_t i = 0; i < order.size(); ++i) {
        if (i) out << ", ";
        std::string title = order[i];
        title[0] = static_cast<char>(std::toupper(static_cast<unsigned char>(title[0])));
        out << title;
    }
    return out.str();
}

void root_fields(std::map<std::string, std::string> &fields, int font_size,
                 int alignment) {
    fields = {{"fontname", "Arial"}, {"fontsize", std::to_string(font_size)},
              {"primarycolour", "&H00FFFFFF"}, {"secondarycolour", "&H000000FF"},
              {"outlinecolour", "&H00101010"}, {"backcolour", "&H80000000"},
              {"bold", "0"}, {"italic", "0"}, {"underline", "0"}, {"strikeout", "0"},
              {"scalex", "80"}, {"scaley", "100"}, {"spacing", "0"}, {"angle", "0"},
              {"borderstyle", "1"}, {"outline", "3"}, {"shadow", "2"},
              {"alignment", std::to_string(alignment)}, {"marginl", "80"},
              {"marginr", "80"}, {"marginv", "70"}, {"encoding", "1"}};
}

std::map<std::string, std::string> derived_fields(const std::string &name,
                                                  std::map<std::string, std::string> overrides = {}) {
    std::map<std::string, std::string> fields;
    for (const std::string &key : kStyleFields) fields[key] = "*";
    fields["name"] = name;
    for (auto &[key, value] : overrides) fields[key] = value;
    return fields;
}

std::string style_line(const std::map<std::string, std::string> &fields,
                       const std::vector<std::string> &order) {
    std::ostringstream out;
    out << "Style: ";
    for (std::size_t i = 0; i < order.size(); ++i) {
        if (i) out << ',';
        const auto found = fields.find(order[i]);
        if (found != fields.end()) out << found->second;
    }
    return out.str();
}

std::string event_text(int index, const std::string &lang_style,
                       const std::string &title_style) {
    static const std::array<const char *, 6> phrases = {
        "The\\hquick brown fox jumps high",
        "A\\{cut\\} move for the final frame",
        "Subtitle audit catches overflow",
        "Quick brown fox jumps over dogs",
        "Template to language, one chain",
        "The quick brown fox jumps"};
    std::ostringstream out;
    out << "{\\fs86\\bord3\\an2\\shad2\\fad(100,100)";
    if (index < 5) out << "\\b1";
    if (index < 7) out << "\\r" << title_style;
    if (index == 5) out << "\\zzz";
    if (index == 6) out << "\\qwe";
    if (index == 7) out << "\\bad\\t(0,800,\\fs80)";
    if (index == 8) out << "\\nope\\t(100,900,\\fs76)";
    if (index == 9) out << "\\t(200,1000,\\fs84)";
    if (index >= 10 && index <= 30) out << "\\fscx95";
    if (index >= 31) out << "\\fscy95";
    out << '}';
    (void)lang_style;
    if (index < 41) out << phrases[static_cast<std::size_t>(index) % phrases.size()];
    return out.str();
}

std::string make_episode(int number, std::uint64_t seed) {
    const std::string ep = "ep" + (std::ostringstream() << std::setw(2)
        << std::setfill('0') << number).str();
    const std::string lang = ep + "_zh";
    std::map<std::string, std::map<std::string, std::string>> styles;
    std::map<std::string, std::string> template_fields;
    root_fields(template_fields, 86, 2);
    template_fields["name"] = "template";
    styles["template"] = template_fields;
    styles["season1"] = derived_fields("season1", {{"outline", "4"}, {"marginl", ""}});
    styles[ep] = derived_fields(ep, {{"fontsize", "*"}, {"marginv", "80"}});
    styles[lang] = derived_fields(lang, {{"primarycolour", "&H00E8F6FF"}});
    std::map<std::string, std::string> title_fields;
    root_fields(title_fields, 76, 8);
    title_fields["name"] = "template_title";
    styles["template_title"] = title_fields;
    styles["season1_title"] = derived_fields("season1_title");
    styles[ep + "_title"] = derived_fields(ep + "_title", {{"spacing", "1"}});
    styles[lang + "_title"] = derived_fields(lang + "_title");
    std::map<std::string, std::string> sign_fields;
    root_fields(sign_fields, 56, 5);
    sign_fields["name"] = "template_sign";
    styles["template_sign"] = sign_fields;
    const std::array<std::string, 9> order = {
        "template", "season1", ep, lang, "template_title", "season1_title",
        ep + "_title", lang + "_title", "template_sign"};
    const auto parents = std::map<std::string, std::string>{
        {"season1", "template"}, {ep, "season1"}, {lang, ep},
        {"template_title", ""}, {"season1_title", "template_title"},
        {ep + "_title", "season1_title"}, {lang + "_title", ep + "_title"},
        {"template_sign", ""}};

    std::vector<std::string> style_order = kStyleFields;
    if (number % 3 == 0) std::rotate(style_order.begin(), style_order.begin() + 4, style_order.end());
    std::vector<std::string> ev_order = kEventFields;
    if (number % 4 == 0) {
        ev_order = {"start", "end", "layer", "style", "name", "marginl", "marginr",
                    "marginv", "effect", "text"};
    }
    std::ostringstream out;
    out << "[Script Info]\nScriptType: v4.00+\nPlayResX: 1920\nPlayResY: 1080\n"
           "WrapStyle: 2\nScaledBorderAndShadow: yes\n"
           "Title: deterministic fixture seed " << seed << "\n\n";
    out << "[V4+ Styles]\n" << field_header("Format", style_order) << '\n';
    for (const std::string &name : order) {
        const auto parent = parents.find(name);
        if (parent != parents.end() && !parent->second.empty())
            out << ";Parent: " << name << '=' << parent->second << '\n';
    }
    out << "; Format is intentionally rotated in some episodes.\n";
    for (const std::string &name : order) out << style_line(styles[name], style_order) << '\n';
    out << "\n[Events]\n" << field_header("Format", ev_order) << '\n';
    for (int index = 0; index < 48; ++index) {
        const std::string style = index >= 39 && index <= 40 ? lang + "_title"
                                : index >= 45 ? ep : lang;
        std::string start = "0:00:01.00", end = "0:00:02.00";
        if (index == 45) { start = "0:00:04.00"; end = "0:00:05.00"; }
        if (index == 46) { start = "0:00:00.00"; end = "0:00:00.90"; }
        if (index == 47) { start = "0:00:06.00"; end = "0:00:07.00"; }
        const std::map<std::string, std::string> values = {
            {"layer", "0"}, {"start", start}, {"end", end}, {"style", style},
            {"name", "Line"}, {"marginl", "0"}, {"marginr", "0"}, {"marginv", "0"},
            {"effect", ""}, {"text", event_text(index, lang, lang + "_title")}};
        out << "Dialogue: ";
        for (std::size_t field = 0; field < ev_order.size(); ++field) {
            if (field) out << ',';
            out << values.at(ev_order[field]);
        }
        out << '\n';
    }
    return out.str();
}

}  // namespace

bool generate_fixtures(const FixtureOptions &options, std::string &error) {
    std::error_code ec;
    std::filesystem::create_directories(options.output_dir, ec);
    if (ec) { error = "cannot create fixture directory"; return false; }
    for (int number = 1; number <= options.count; ++number) {
        std::ostringstream name;
        name << options.output_dir << "/ep" << std::setw(2) << std::setfill('0')
             << number << "_zh.ass";
        std::string content = make_episode(number, options.seed);
        if (number % 2 == 1) {
            std::string crlf;
            for (char ch : content) {
                if (ch == '\n') crlf += '\r';
                crlf += ch;
            }
            content = std::move(crlf);
        }
        if (number == 1) content.insert(0, "\xEF\xBB\xBF");
        std::ofstream file(name.str(), std::ios::binary);
        if (!file || !(file << content)) {
            error = "cannot write " + name.str();
            return false;
        }
    }
    return true;
}

}  // namespace audit
