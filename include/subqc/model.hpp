#pragma once

#include <cstdint>
#include <map>
#include <optional>
#include <string>
#include <vector>

namespace subqc {

struct Diagnostic {
    std::string severity;
    std::string file;
    int line = 0;
    std::string code;
    std::string message;
};

struct ScriptInfo {
    int play_res_x = 384;
    int play_res_y = 288;
    bool play_res_missing = true;
    std::string wrap_style;
    std::string scaled_border;
};

struct RawStyle {
    std::string name;
    std::map<std::string, std::string> fields;
    int line = 0;
};

struct Event {
    int layer = 0;
    std::string start_text;
    std::string end_text;
    double start_ms = 0;
    double end_ms = 0;
    std::string style;
    std::string name;
    int margin_l = 0;
    int margin_r = 0;
    int margin_v = 0;
    std::string effect;
    std::string text;
    int line = 0;
    int input_index = 0;
};

struct AssFile {
    std::string path;
    ScriptInfo info;
    std::vector<RawStyle> styles;
    std::vector<Event> events;
    std::vector<Diagnostic> diagnostics;
    bool fatal = false;
};

struct StyleValues {
    std::string parent;
    std::string font_name;
    std::optional<double> font_size;
    std::optional<std::string> primary_colour;
    std::optional<std::string> secondary_colour;
    std::optional<std::string> outline_colour;
    std::optional<std::string> back_colour;
    std::optional<double> outline;
    std::optional<double> shadow;
    std::optional<int> margin_l;
    std::optional<int> margin_r;
    std::optional<int> margin_v;
    std::optional<int> alignment;
    std::optional<int> bold;
    std::optional<int> italic;
    std::optional<double> scale_x;
    std::optional<double> scale_y;
    std::optional<double> spacing;
    std::optional<double> angle;
    std::optional<int> border_style;
};

struct MergedStyle {
    std::string font_name = "SubQC Sans";
    double font_size = 36.0;
    std::string primary_colour = "&H00FFFFFF";
    std::string secondary_colour = "&H000000FF";
    std::string outline_colour = "&H00000000";
    std::string back_colour = "&H64000000";
    double outline = 2.0;
    double shadow = 0.0;
    int margin_l = 20;
    int margin_r = 20;
    int margin_v = 20;
    int alignment = 2;
    int bold = 0;
    int italic = 0;
    double scale_x = 100.0;
    double scale_y = 100.0;
    double spacing = 0.0;
    double angle = 0.0;
    int border_style = 1;
};

struct ResolvedStyle {
    std::string name;
    std::string parent;
    std::vector<std::string> chain;
    MergedStyle merged;
    bool valid = true;
    bool cycle = false;
    bool parent_fallback = false;
    bool duplicate_override = false;
    double font_size_px = 0.0;
    double outline_px = 0.0;
};

struct StyleCatalog {
    std::map<std::string, ResolvedStyle> resolved;
    std::vector<std::string> order;
    std::vector<Diagnostic> diagnostics;
    int definitions = 0;
    int duplicate_definitions = 0;
};

struct InlineSegment {
    std::string text;
    std::string style;
    int glyphs = 0;
};

struct InlineAnalysis {
    std::map<std::string, int> tag_counts;
    std::map<std::string, int> unknown_tag_counts;
    std::vector<InlineSegment> segments;
    std::vector<Diagnostic> diagnostics;
    int glyphs = 0;
    bool malformed = false;
};

struct AnalyzedEvent {
    Event event;
    InlineAnalysis inline_tags;
    std::string base_style;
    std::string effective_style;
};

struct Bounds {
    int x0 = 0;
    int y0 = 0;
    int x1 = 0;
    int y1 = 0;
    int width() const { return x1 > x0 ? x1 - x0 : 0; }
    int height() const { return y1 > y0 ? y1 - y0 : 0; }
    bool empty() const { return width() == 0 || height() == 0; }
};

struct RenderResult {
    Bounds bounds;
    int glyphs = 0;
    std::uint32_t crc32 = 0;
    bool rendered = false;
};

struct RenderOptions {
    int width = 1920;
    int height = 1080;
    std::string font_dir;
    std::string font_family;
    int safe_x = 0;
    int safe_y = 0;
    int alpha_threshold = 1;
};

struct AnalyzedFile {
    AssFile ass;
    StyleCatalog catalog;
    std::vector<AnalyzedEvent> events;
    std::vector<RenderResult> renders;
    bool load_failed = false;
};

}  // namespace subqc
