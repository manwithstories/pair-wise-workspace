#include "subtitle_audit/layout_probe.hpp"

#include <algorithm>
#include <climits>
#include <cstdarg>
#include <cstdlib>
#include <filesystem>
#include <iomanip>
#include <map>
#include <set>
#include <sstream>
#include <unordered_set>

#include <fontconfig/fontconfig.h>
#include <ass/ass.h>

#include "subtitle_audit/style_chain.hpp"
#include "subtitle_audit/tag_lexer.hpp"

namespace audit {
namespace {

void quiet_message(int, const char *, va_list, void *) {}

std::string field(const std::map<std::string, std::string> &fields,
                  const std::string &key, const std::string &fallback) {
    const auto it = fields.find(key);
    return it == fields.end() || it->second.empty() ? fallback : it->second;
}

std::string no_commas(std::string value) {
    std::replace(value.begin(), value.end(), ',', ' ');
    return value;
}

std::string resolved_style_line(const ResolvedStyle &style) {
    const auto &f = style.fields;
    std::ostringstream line;
    line << "Style: " << no_commas(style.name)
         << ',' << no_commas(field(f, "fontname", "Arial"))
         << ',' << field(f, "fontsize", "48")
         << ',' << field(f, "primarycolour", "&H00FFFFFF")
         << ',' << field(f, "secondarycolour", "&H000000FF")
         << ',' << field(f, "outlinecolour", "&H00000000")
         << ',' << field(f, "backcolour", "&H80000000")
         << ',' << field(f, "bold", "0") << ',' << field(f, "italic", "0")
         << ',' << field(f, "underline", "0") << ',' << field(f, "strikeout", "0")
         << ',' << field(f, "scalex", "100") << ',' << field(f, "scaley", "100")
         << ',' << field(f, "spacing", "0") << ',' << field(f, "angle", "0")
         << ',' << field(f, "borderstyle", "1") << ',' << field(f, "outline", "2")
         << ',' << field(f, "shadow", "0") << ',' << field(f, "alignment", "2")
         << ',' << field(f, "marginl", "20") << ',' << field(f, "marginr", "20")
         << ',' << field(f, "marginv", "30") << ',' << field(f, "encoding", "1")
         << '\n';
    return line.str();
}

std::string normalized_script(const AssDocument &doc, const ChainAnalysis &chains,
                              int event_index) {
    std::ostringstream out;
    out << "[Script Info]\nScriptType: v4.00+\n";
    out << "PlayResX: " << field(doc.info, "playresx", "1920") << '\n';
    out << "PlayResY: " << field(doc.info, "playresy", "1080") << '\n';
    out << "WrapStyle: " << field(doc.info, "wrapstyle", "0") << '\n';
    out << "ScaledBorderAndShadow: yes\n\n[V4+ Styles]\n";
    out << "Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, "
           "OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, "
           "ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, "
           "MarginL, MarginR, MarginV, Encoding\n";
    for (const Style &style : doc.styles) {
        const auto found = chains.styles.find(style.name);
        if (found != chains.styles.end()) out << resolved_style_line(found->second);
    }
    out << "\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, "
           "MarginV, Effect, Text\n";
    for (std::size_t i = 0; i < doc.events.size(); ++i) {
        if (event_index >= 0 && static_cast<std::size_t>(event_index) != i) continue;
        const Event &event = doc.events[i];
        out << "Dialogue: " << event.layer << ',' << event.start << ',' << event.end
            << ',' << no_commas(event.style) << ',' << no_commas(event.name)
            << ",0,0,0,," << event.text << '\n';
    }
    return out.str();
}

int positive_int(const std::string &value, int fallback) {
    try {
        const int parsed = std::stoi(value);
        return parsed > 0 ? parsed : fallback;
    } catch (...) {
        return fallback;
    }
}

std::unordered_set<std::string> font_families() {
    std::unordered_set<std::string> families;
    const auto cache = std::filesystem::temp_directory_path() / "subtitle-audit-fontcache";
    std::error_code ec;
    std::filesystem::create_directories(cache, ec);
    if (!ec) setenv("XDG_CACHE_HOME", cache.c_str(), 1);
    FcConfig *config = FcInitLoadConfigAndFonts();
    if (!config) return families;
    FcFontSet *set = FcConfigGetFonts(config, FcSetSystem);
    if (!set) { FcConfigDestroy(config); return families; }
    for (int i = 0; i < set->nfont; ++i) {
        FcChar8 *family = nullptr;
        for (int index = 0;
             FcPatternGetString(set->fonts[i], FC_FAMILY, index, &family) == FcResultMatch;
             ++index) {
            std::string name = lower_ascii(reinterpret_cast<const char *>(family));
            if (!name.empty() && name.front() == '@') name.erase(name.begin());
            families.insert(name);
        }
    }
    FcConfigDestroy(config);
    return families;
}

std::vector<std::string> event_missing_fonts(const ChainAnalysis &chains,
                                             const Event &event,
                                             const std::unordered_set<std::string> &families) {
    std::set<std::string> styles{event.style};
    std::set<std::string> inline_fonts;
    const LexedEvent lexed = lex_event_text(event.text);
    for (const TagToken &tag : lexed.tags) {
        if (tag.name == "r" && !trim(tag.argument).empty()) styles.insert(trim(tag.argument));
        if (tag.name == "fn" && !trim(tag.argument).empty()) inline_fonts.insert(trim(tag.argument));
    }
    std::set<std::string> missing;
    const auto check_font = [&](const std::string &raw) {
        std::string name = lower_ascii(raw);
        if (!name.empty() && name.front() == '@') name.erase(name.begin());
        if (!name.empty() && !families.count(name)) missing.insert(raw);
    };
    for (const std::string &style_name : styles) {
        const auto found = chains.styles.find(style_name);
        if (found == chains.styles.end()) continue;
        check_font(field(found->second.fields, "fontname", "Arial"));
    }
    for (const std::string &font : inline_fonts) check_font(font);
    return {missing.begin(), missing.end()};
}

}  // namespace

ProbeResult render_probe(const AssDocument &doc, int64_t frame,
                         const std::string &dump_alpha_dir) {
    ProbeResult result;
    result.frame = frame;
    result.width = doc.info.count("playresx") ? positive_int(doc.info.at("playresx"), 1920) : 1920;
    result.height = doc.info.count("playresy") ? positive_int(doc.info.at("playresy"), 1080) : 1080;
    const ChainAnalysis chains = analyze_style_chains(doc);
    if (!chains.ok()) {
        result.ok = false;
        result.error = chains.errors.empty() ? "style chain failure"
                                             : chains.errors.front().message;
        return result;
    }
    const auto families = font_families();
    std::set<std::string> all_missing;

    ASS_Library *library = ass_library_init();
    if (!library) {
        result.ok = false;
        result.error = "libass library initialization failed";
        return result;
    }
    ass_set_message_cb(library, quiet_message, nullptr);
    ASS_Renderer *renderer = ass_renderer_init(library);
    if (!renderer) {
        ass_library_done(library);
        result.ok = false;
        result.error = "libass renderer initialization failed";
        return result;
    }
    ass_set_frame_size(renderer, result.width, result.height);
    ass_set_storage_size(renderer, result.width, result.height);
    ass_set_fonts(renderer, nullptr, "Arial", ASS_FONTPROVIDER_AUTODETECT, nullptr, 0);
    ass_set_cache_limits(renderer, 0, 64);

    for (std::size_t index = 0; index < doc.events.size(); ++index) {
        const Event &event = doc.events[index];
        ProbeEvent probe;
        probe.index = static_cast<int>(index);
        probe.line = event.line;
        probe.missing_fonts = families.empty()
            ? std::vector<std::string>{}
            : event_missing_fonts(chains, event, families);
        for (const std::string &font : probe.missing_fonts) all_missing.insert(font);

        std::string script = normalized_script(doc, chains, static_cast<int>(index));
        ASS_Track *track = ass_read_memory(library, script.data(), script.size(), nullptr);
        if (!track) {
            ass_renderer_done(renderer);
            ass_library_done(library);
            result.ok = false;
            result.error = "libass failed to load normalized track for event " +
                           std::to_string(index + 1);
            return result;
        }
        int change = 0;
        ASS_Image *images = ass_render_frame(renderer, track, frame, &change);
        std::vector<AlphaPlane> planes;
        int min_x = INT_MAX, min_y = INT_MAX, max_x = INT_MIN, max_y = INT_MIN;
        for (ASS_Image *image = images; image; image = image->next) {
            if (image->w <= 0 || image->h <= 0 || !image->bitmap) continue;
            min_x = std::min(min_x, image->dst_x);
            min_y = std::min(min_y, image->dst_y);
            max_x = std::max(max_x, image->dst_x + image->w);
            max_y = std::max(max_y, image->dst_y + image->h);
            planes.push_back({image->bitmap, image->w, image->h, image->stride,
                              image->dst_x, image->dst_y});
        }
        if (!planes.empty()) {
            const int left = std::clamp(min_x, 0, result.width);
            const int top = std::clamp(min_y, 0, result.height);
            const int right = std::clamp(max_x, 0, result.width);
            const int bottom = std::clamp(max_y, 0, result.height);
            if (right > left && bottom > top) {
                probe.visible = true;
                probe.bbox = {left, top, right - left, bottom - top};
                probe.alpha_fnv1a = alpha_digest(planes);
                result.max_width = std::max(result.max_width, probe.bbox.w);
                result.max_height = std::max(result.max_height, probe.bbox.h);
                ++result.visible;
                if (!dump_alpha_dir.empty()) {
                    std::ostringstream name;
                    name << dump_alpha_dir << "/event_" << std::setw(4) << std::setfill('0')
                         << (index + 1) << ".ppm";
                    if (!write_alpha_ppm(name.str(), planes, probe.bbox)) {
                        ass_free_track(track);
                        ass_renderer_done(renderer);
                        ass_library_done(library);
                        result.ok = false;
                        result.error = "failed to write alpha PPM";
                        return result;
                    }
                }
            }
        }
        result.events.push_back(std::move(probe));
        ass_free_track(track);
    }
    ass_renderer_done(renderer);
    ass_library_done(library);
    result.missing_fonts.assign(all_missing.begin(), all_missing.end());
    return result;
}

}  // namespace audit
