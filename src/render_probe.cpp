#include "subqc/render_probe.hpp"

#include <ass/ass.h>
#include <ft2build.h>
#include FT_FREETYPE_H

#include <algorithm>
#include <cstdarg>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <limits>
#include <sstream>
#include <string>

#include "subqc/ass_reader.hpp"

namespace subqc {
namespace {

std::string format_number(double value) {
    std::ostringstream out;
    out.setf(std::ios::fixed);
    out.precision(3);
    out << value;
    std::string text = out.str();
    while (text.size() > 1 && text.back() == '0') text.pop_back();
    if (text.back() == '.') text.pop_back();
    return text;
}

std::string ass_time(double milliseconds) {
    const long long total = std::max<long long>(0, static_cast<long long>(milliseconds));
    const long long hours = total / 3600000;
    const long long minutes = (total / 60000) % 60;
    const long long seconds = (total / 1000) % 60;
    const long long centis = (total / 10) % 100;
    char buffer[32];
    std::snprintf(buffer, sizeof(buffer), "%lld:%02lld:%02lld.%02lld", hours, minutes, seconds, centis);
    return buffer;
}

std::string safe_name(const std::string &value) {
    std::string result = value;
    for (char &ch : result) {
        if (ch == ',' || ch == '\n' || ch == '\r') ch = '_';
    }
    return result.empty() ? "Default" : result;
}

std::string style_row(const std::string &name, const MergedStyle &style) {
    std::ostringstream out;
    out << "Style: " << safe_name(name) << ',' << style.font_name << ',' << format_number(style.font_size)
        << ',' << style.primary_colour << ',' << style.secondary_colour << ',' << style.outline_colour
        << ',' << style.back_colour << ',' << style.bold << ',' << style.italic << ",0,0,"
        << format_number(style.scale_x) << ',' << format_number(style.scale_y) << ','
        << format_number(style.spacing) << ',' << format_number(style.angle) << ',' << style.border_style
        << ',' << format_number(style.outline) << ',' << format_number(style.shadow) << ','
        << style.alignment << ',' << style.margin_l << ',' << style.margin_r << ',' << style.margin_v
        << ",1\n";
    return out.str();
}

std::string make_script(const AnalyzedFile &file) {
    const auto &info = file.ass.info;
    std::ostringstream out;
    out << "[Script Info]\nScriptType: v4.00+\nPlayResX: " << info.play_res_x
        << "\nPlayResY: " << info.play_res_y << "\nWrapStyle: 2\nScaledBorderAndShadow: yes\n\n"
        << "[V4+ Styles]\n"
        << "Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, "
           "BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, "
           "BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n";
    bool has_default = false;
    for (const auto &key : file.catalog.order) {
        const auto &style = file.catalog.resolved.at(key);
        if (key == "default") has_default = true;
        out << style_row(style.name, style.valid ? style.merged : MergedStyle{});
    }
    if (!has_default) out << style_row("Default", MergedStyle{});
    out << "\n[Events]\n"
        << "Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n";
    for (std::size_t i = 0; i < file.events.size(); ++i) {
        const auto &event = file.events[i];
        const std::string start = ass_time(static_cast<double>(i) * 2000.0);
        const std::string end = ass_time(static_cast<double>(i) * 2000.0 + 1500.0);
        const auto found = file.catalog.resolved.find(lower_copy(event.base_style));
        const std::string style = found != file.catalog.resolved.end()
                                      ? safe_name(found->second.name)
                                      : safe_name(event.base_style);
        out << "Dialogue: " << event.event.layer << ',' << start << ',' << end << ',' << style
            << ",,0,0,0,," << event.event.text << '\n';
    }
    return out.str();
}

unsigned char alpha(const ASS_Image *image, int x, int y) {
    const unsigned int value = image->bitmap[y * image->stride + x];
    const unsigned int opacity = 255u - (image->color & 0xFFu);
    return static_cast<unsigned char>((value * opacity + 127u) / 255u);
}

}  // namespace

struct RenderProbe::Impl {
    ASS_Library *library = nullptr;
    std::string default_font;
    std::string family;
};

std::uint32_t crc32_bytes(const std::uint8_t *data, std::size_t size) {
    std::uint32_t crc = 0xFFFFFFFFu;
    for (std::size_t i = 0; i < size; ++i) {
        crc ^= data[i];
        for (int bit = 0; bit < 8; ++bit)
            crc = (crc >> 1) ^ (0xEDB88320u & (0u - (crc & 1u)));
    }
    return crc ^ 0xFFFFFFFFu;
}

RenderProbe::RenderProbe(std::unique_ptr<Impl> impl) : impl_(std::move(impl)) {}
RenderProbe::~RenderProbe() {
    if (impl_ && impl_->library) ass_library_done(impl_->library);
}
RenderProbe::RenderProbe(RenderProbe &&) noexcept = default;
RenderProbe &RenderProbe::operator=(RenderProbe &&) noexcept = default;

std::unique_ptr<RenderProbe> RenderProbe::create(const RenderOptions &options,
                                                 std::vector<Diagnostic> &diagnostics) {
    auto impl = std::make_unique<Impl>();
    impl->library = ass_library_init();
    if (!impl->library) {
        diagnostics.push_back({"error", options.font_dir, 0, "libass_init", "Cannot initialize libass"});
        return nullptr;
    }
    ass_set_message_cb(impl->library, [](int, const char *, va_list, void *) {}, nullptr);

    std::error_code ec;
    if (!std::filesystem::is_directory(options.font_dir, ec)) {
        diagnostics.push_back({"error", options.font_dir, 0, "font_dir", "Font directory does not exist"});
        ass_library_done(impl->library);
        return nullptr;
    }
    std::vector<std::filesystem::path> fonts;
    for (const auto &entry : std::filesystem::recursive_directory_iterator(options.font_dir, ec)) {
        if (!entry.is_regular_file()) continue;
        const std::string ext = lower_copy(entry.path().extension().string());
        if (ext == ".ttf" || ext == ".otf" || ext == ".ttc" || ext == ".otc")
            fonts.push_back(entry.path());
    }
    std::sort(fonts.begin(), fonts.end());
    if (fonts.empty()) {
        diagnostics.push_back({"error", options.font_dir, 0, "font_empty", "No font files in font directory"});
        ass_library_done(impl->library);
        return nullptr;
    }

    FT_Library freetype = nullptr;
    FT_Face face = nullptr;
    if (FT_Init_FreeType(&freetype) == 0 &&
        FT_New_Face(freetype, fonts.front().string().c_str(), 0, &face) == 0) {
        if (face->family_name) impl->family = face->family_name;
        FT_Done_Face(face);
    }
    if (freetype) FT_Done_FreeType(freetype);
    if (!options.font_family.empty()) impl->family = options.font_family;
    if (impl->family.empty()) impl->family = "SubQC Sans";
    impl->default_font = fonts.front().string();

    for (const auto &font : fonts) {
        std::ifstream input(font, std::ios::binary);
        std::ostringstream buffer;
        buffer << input.rdbuf();
        const std::string bytes = buffer.str();
        if (bytes.empty() || bytes.size() > static_cast<std::size_t>(std::numeric_limits<int>::max())) {
            diagnostics.push_back({"warning", font.string(), 0, "font_read", "Skipping unreadable font file"});
            continue;
        }
        ass_add_font(impl->library, font.filename().string().c_str(), bytes.data(),
                     static_cast<int>(bytes.size()));
    }
    return std::unique_ptr<RenderProbe>(new RenderProbe(std::move(impl)));
}

bool RenderProbe::render_file(AnalyzedFile &file, const RenderOptions &options) {
    file.renders.assign(file.events.size(), RenderResult{});
    if (!impl_ || !impl_->library || options.width <= 0 || options.height <= 0) return false;
    std::string script = make_script(file);
    ASS_Renderer *renderer = ass_renderer_init(impl_->library);
    ASS_Track *track = ass_new_track(impl_->library);
    if (!renderer || !track) {
        if (renderer) ass_renderer_done(renderer);
        if (track) ass_free_track(track);
        file.ass.diagnostics.push_back({"error", file.ass.path, 0, "render_init",
                                        "Cannot create libass renderer/track"});
        return false;
    }
    ass_set_frame_size(renderer, options.width, options.height);
    ass_set_storage_size(renderer, options.width, options.height);
    ass_set_fonts(renderer, impl_->default_font.c_str(), impl_->family.c_str(), ASS_FONTPROVIDER_NONE,
                  nullptr, 1);
    ass_process_data(track, script.data(), static_cast<int>(script.size()));
    if (track->n_events != static_cast<int>(file.events.size())) {
        file.ass.diagnostics.push_back({"warning", file.ass.path, 0, "render_track",
                                        "Canonical track event count differs from parsed count"});
    }

    std::vector<std::uint8_t> canvas(static_cast<std::size_t>(options.width) * options.height, 0);
    for (std::size_t index = 0; index < file.events.size(); ++index) {
        std::fill(canvas.begin(), canvas.end(), 0);
        int changed = 0;
        const double start = static_cast<double>(index) * 2000.0;
        ASS_Image *images = ass_render_frame(renderer, track, static_cast<long long>(start + 300.0), &changed);
        Bounds bounds;
        bool found = false;
        for (ASS_Image *image = images; image; image = image->next) {
            if (image->w <= 0 || image->h <= 0 || !image->bitmap) continue;
            for (int y = 0; y < image->h; ++y) {
                const int py = image->dst_y + y;
                if (py < 0 || py >= options.height) continue;
                for (int x = 0; x < image->w; ++x) {
                    const int px = image->dst_x + x;
                    if (px < 0 || px >= options.width) continue;
                    const unsigned char a = alpha(image, x, y);
                    if (a < options.alpha_threshold) continue;
                    std::uint8_t &pixel = canvas[static_cast<std::size_t>(py) * options.width + px];
                    pixel = std::max(pixel, a);
                    if (!found) {
                        bounds = {px, py, px + 1, py + 1};
                        found = true;
                    } else {
                        bounds.x0 = std::min(bounds.x0, px);
                        bounds.y0 = std::min(bounds.y0, py);
                        bounds.x1 = std::max(bounds.x1, px + 1);
                        bounds.y1 = std::max(bounds.y1, py + 1);
                    }
                }
            }
        }
        RenderResult &result = file.renders[index];
        result.bounds = bounds;
        result.glyphs = file.events[index].inline_tags.glyphs;
        result.rendered = found;
        result.crc32 = crc32_bytes(canvas.data(), canvas.size());
    }
    ass_free_track(track);
    ass_renderer_done(renderer);
    return true;
}

}  // namespace subqc

