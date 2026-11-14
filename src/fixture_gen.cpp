#include "subqc/fixture_gen.hpp"

#include <algorithm>
#include <cstdio>
#include <fstream>
#include <iomanip>
#include <random>
#include <sstream>
#include <vector>

#include "nlohmann/json.hpp"
#include "subqc/mini_font.hpp"

namespace subqc {
namespace {

struct StylePlan {
    std::string name;
    std::string parent;
    double size = 32.0;
    double outline = 2.0;
    std::string colour = "&H00FFFFFF";
};

struct FixtureFile {
    std::string body;
    int play_x = 384;
    int play_y = 288;
    bool missing_playres = false;
    std::vector<std::string> event_styles;
    std::string reset_style;
};

std::string number(double value) {
    std::ostringstream out;
    out << std::fixed << std::setprecision(2) << value;
    std::string text = out.str();
    while (text.size() > 1 && text.back() == '0') text.pop_back();
    if (text.back() == '.') text.pop_back();
    return text;
}

std::string colour(unsigned index) {
    static const std::vector<std::string> colours = {
        "&H00FFFFFF", "&H0000FFFF", "&H00FFB0C0", "&H00C0FFC0", "&H00E0E0E0"};
    return colours[index % colours.size()];
}

std::string style_line(const StylePlan &style, double scale) {
    std::ostringstream out;
    out << "Style: " << style.name << ",SubQC Sans," << number(style.size * scale) << ","
        << style.colour << ",&H000000FF,&H00000000,&H64000000,0,0,0,0,100,100,0,0,1,"
        << number(style.outline * scale) << ",0,2," << static_cast<int>(30 * scale) << ','
        << static_cast<int>(30 * scale) << ',' << static_cast<int>(18 * scale) << ",1,"
        << style.parent << '\n';
    return out.str();
}

FixtureFile make_file(int file_index, std::uint64_t seed) {
    FixtureFile file;
    const double scale = file_index % 3 == 1 ? 1.25 : (file_index % 3 == 2 ? 2.5 : 1.0);
    file.play_x = file_index % 3 == 1 ? 640 : (file_index % 3 == 2 ? 1280 : 384);
    file.play_y = file_index % 3 == 1 ? 360 : (file_index % 3 == 2 ? 720 : 288);
    file.missing_playres = file_index == 4;
    if (file.missing_playres) {
        file.play_x = 384;
        file.play_y = 288;
    }
    std::vector<StylePlan> styles;
    const std::string base_colour = colour(static_cast<unsigned>(seed / 7 + file_index));
    styles.push_back({"Default", "", 32, 2, "&H00FFFFFF"});
    styles.push_back({"Base", "Default", 34, 2.5, base_colour});
    if (file_index < 2) {
        styles.push_back({"RoleCommon", "Base", 36, 3, "&H00FFFF00"});
        styles.push_back({"Hero", "RoleCommon", 40, 3.5, "&H0000FFFF"});
        file.event_styles = {"Default", "Base", "RoleCommon", "Hero"};
        file.reset_style = "Hero";
    } else if (file_index == 10) {
        styles.push_back({"Lead", "GhostParent", 36, 3, "&H00C0FFC0"});
        file.event_styles = {"Default", "Base", "Lead"};
        file.reset_style = "Lead";
    } else if (file_index == 11) {
        styles.erase(std::remove_if(styles.begin(), styles.end(), [](const StylePlan &style) {
            return style.name == "Base";
        }), styles.end());
        styles.push_back({"LoopA", "LoopB", 36, 3, "&H00FF00FF"});
        styles.push_back({"LoopB", "LoopA", 36, 3, "&H00FF00FF"});
        file.event_styles = {"Default"};
        file.reset_style = "Default";
    } else {
        styles.push_back({"Lead", "Base", 36, 3, "&H00C0FFC0"});
        file.event_styles = {"Default", "Base", "Lead"};
        file.reset_style = "Lead";
    }
    std::ostringstream out;
    out << "[Script Info]\nScriptType: v4.00+\n";
    if (!file.missing_playres)
        out << "PlayResX: " << file.play_x << "\nPlayResY: " << file.play_y << "\n";
    out << "WrapStyle: 2\nScaledBorderAndShadow: yes\n\n[V4+ Styles]\n"
        << "Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, "
           "BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, "
           "BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding, ParentStyle\n";
    for (const auto &style : styles) out << style_line(style, scale);
    if (file_index == 1) out << style_line({"Hero", "RoleCommon", 42, 3.5, "&H0000FF00"}, scale);
    out << "\n[Events]\n"
        << "Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n";
    file.body = out.str();
    return file;
}

std::string event_text(int global_index, const FixtureFile &file, const std::string &reset_style,
                       std::mt19937_64 &random) {
    static const std::vector<std::string> words = {
        "ALF", "BRV", "CHR", "DLT", "ECH", "FOX", "GLF", "HTL"};
    std::ostringstream text;
    const bool animation = global_index % 15 == 0 || global_index == 239;
    const bool reset = global_index % 6 == 0 || global_index == 239;
    if (global_index % 5 < 2)
        text << "{\\fs" << static_cast<int>(30.0 * file.play_y / 288.0) + global_index % 5 << '}';
    text << 'L' << std::setw(3) << std::setfill('0') << global_index << ' '
         << words[random() % words.size()];
    if (reset && !(animation && global_index % 30 == 0)) text << "{\\r" << reset_style << '}';
    if (reset) text << 'R';
    if (animation) {
        if (global_index % 30 == 0)
            text << "{\\t(0,400,\\r" << reset_style << "\\fscx120)}T";
        else
            text << "{\\t(0,400,\\fscx120)}T";
    }
    if (global_index % 3 == 0) text << "{\\b1}B";
    if (global_index % 4 == 0)
        text << "{\\pos(" << file.play_x / 2 << ',' << file.play_y / 2 << ")\\an5}M";
    if (global_index % 7 == 0) text << "{\\fad(0,1000)}F";
    if (global_index % 13 == 0) text << "{\\zzz9}U";
    if (global_index % 17 == 0) text << "\\{L}";
    if (global_index == 3 || global_index == 37 || global_index == 119)
        text << "{\\pos(10,10)\\an7}E";
    return text.str();
}

std::string time_text(int centiseconds) {
    const int minutes = centiseconds / 6000;
    const int seconds = (centiseconds / 100) % 60;
    const int fraction = centiseconds % 100;
    char buffer[32];
    std::snprintf(buffer, sizeof(buffer), "0:%02d:%02d.%02d", minutes, seconds, fraction);
    return buffer;
}

bool write_text_file(const std::filesystem::path &path, const std::string &text, bool bom, bool crlf,
                     std::string &error) {
    std::ofstream output(path, std::ios::binary);
    if (!output) {
        error = "Cannot write fixture: " + path.string();
        return false;
    }
    if (bom) output.write("\xEF\xBB\xBF", 3);
    if (crlf) {
        for (std::size_t i = 0; i < text.size(); ++i) {
            if (text[i] == '\n') output.write("\r\n", 2);
            else output.put(text[i]);
        }
    } else {
        output.write(text.data(), static_cast<std::streamsize>(text.size()));
    }
    if (!output) {
        error = "Cannot finalize fixture: " + path.string();
        return false;
    }
    return true;
}

}  // namespace

bool generate_fixtures(const std::filesystem::path &out, int event_count, std::uint64_t seed,
                       FixtureSummary &summary, std::string &error) {
    if (event_count <= 0) {
        error = "event count must be positive";
        return false;
    }
    std::error_code ec;
    std::filesystem::remove_all(out, ec);
    std::filesystem::create_directories(out / "fonts", ec);
    if (ec) {
        error = "Cannot create fixture directory: " + ec.message();
        return false;
    }
    const auto font_path = out / "fonts" / "SubQC-Sans.ttf";
    if (!write_minimal_font(font_path, "SubQC Sans", error)) return false;
    nlohmann::json manifest = {{"family", "SubQC Sans"},
                               {"files", {"SubQC-Sans.ttf"}},
                               {"seed", seed},
                               {"generated_by", "subqc gen-fixtures"}};
    std::ofstream manifest_out(out / "fonts" / "manifest.json");
    manifest_out << manifest.dump(2) << '\n';

    std::vector<FixtureFile> files;
    std::mt19937_64 random(seed);
    for (int i = 0; i < 12; ++i) files.push_back(make_file(i, seed));
    for (int i = 0; i < event_count; ++i) {
        const int file_index = i % 12;
        FixtureFile &file = files[file_index];
        const int local_index = i / 12;
        const std::string style = file.event_styles[local_index % file.event_styles.size()];
        const int start = local_index * 300;
        std::ostringstream event;
        event << "Dialogue: 0," << time_text(start) << ',' << time_text(start + 150) << ',' << style
              << ",,0,0,0,," << event_text(i, file, file.reset_style, random) << '\n';
        file.body += event.str();
    }
    for (int i = 0; i < 12; ++i) {
        std::ostringstream name;
        name << "ep" << std::setw(2) << std::setfill('0') << (i + 1) << ".ass";
        if (!write_text_file(out / name.str(), files[i].body, i == 4, i == 4, error)) return false;
    }
    summary.files = 12;
    summary.events = event_count;
    summary.styles = 38;
    return true;
}

}  // namespace subqc
