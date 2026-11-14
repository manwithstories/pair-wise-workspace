#include "subqc/metrics.hpp"

#include <algorithm>
#include <iomanip>
#include <map>
#include <sstream>

namespace subqc {
namespace {

using json = nlohmann::json;

json diagnostic_json(const Diagnostic &diagnostic) {
    return {{"severity", diagnostic.severity}, {"file", diagnostic.file}, {"line", diagnostic.line},
            {"code", diagnostic.code}, {"message", diagnostic.message}};
}

std::string crc_hex(std::uint32_t value) {
    std::ostringstream out;
    out << std::hex << std::setw(8) << std::setfill('0') << value;
    return out.str();
}

bool outside_safe_area(const Bounds &box, const RenderOptions &options) {
    if (box.empty()) return false;
    return box.x0 < options.safe_x || box.y0 < options.safe_y ||
           box.x1 > options.width - options.safe_x || box.y1 > options.height - options.safe_y;
}

}  // namespace

json build_report(const std::vector<AnalyzedFile> &files, const RenderOptions &options,
                  long long elapsed_ms, const std::vector<Diagnostic> &extra_diagnostics) {
    json report;
    json file_rows = json::array();
    json chains = json::array();
    json renders = json::array();
    json warnings = json::array();
    json errors = json::array();
    std::map<std::string, int> inline_counts = {
        {"r", 0}, {"b", 0}, {"fs", 0}, {"pos", 0}, {"an", 0}, {"fad", 0}, {"t", 0}};
    std::map<std::string, int> unknown_counts;
    int total_events = 0;
    int total_styles = 0;
    int margin_violations = 0;
    int fatal_files = 0;
    int global_event = 0;

    for (const auto &file : files) {
        int file_warnings = 0;
        int file_errors = 0;
        for (const auto &diagnostic : file.ass.diagnostics) {
            auto &target = diagnostic.severity == "error" ? errors : warnings;
            target.push_back(diagnostic_json(diagnostic));
            if (diagnostic.severity == "error") ++file_errors;
            else ++file_warnings;
        }
        if (file.load_failed || file.ass.fatal) ++fatal_files;
        file_rows.push_back({{"path", file.ass.path}, {"fatal", file.ass.fatal}, {"load_failed", file.load_failed},
                             {"events", file.events.size()}, {"styles", file.catalog.resolved.size()},
                             {"warnings", file_warnings}, {"errors", file_errors},
                             {"play_res_x", file.ass.info.play_res_x}, {"play_res_y", file.ass.info.play_res_y}});
        total_events += static_cast<int>(file.events.size());
        total_styles += static_cast<int>(file.catalog.resolved.size());

        for (const auto &key : file.catalog.order) {
            const ResolvedStyle &style = file.catalog.resolved.at(key);
            chains.push_back({{"file", file.ass.path}, {"name", style.name}, {"parent", style.parent},
                              {"chain", style.chain}, {"font_size", style.merged.font_size},
                              {"outline", style.merged.outline}, {"font_size_px", style.font_size_px},
                              {"outline_px", style.outline_px},
                              {"primary_colour", style.merged.primary_colour},
                              {"margin_l", style.merged.margin_l}, {"margin_r", style.merged.margin_r},
                              {"margin_v", style.merged.margin_v}, {"alignment", style.merged.alignment},
                              {"valid", style.valid},
                              {"cycle", style.cycle}, {"parent_fallback", style.parent_fallback}});
        }

        for (std::size_t index = 0; index < file.events.size(); ++index) {
            const auto &event = file.events[index];
            for (const auto &item : event.inline_tags.tag_counts) inline_counts[item.first] += item.second;
            for (const auto &item : event.inline_tags.unknown_tag_counts) unknown_counts[item.first] += item.second;
            const RenderResult result = index < file.renders.size() ? file.renders[index] : RenderResult{};
            const bool violation = outside_safe_area(result.bounds, options);
            if (violation) ++margin_violations;
            json segments = json::array();
            for (const auto &segment : event.inline_tags.segments)
                segments.push_back({{"text", segment.text}, {"style", segment.style}, {"glyphs", segment.glyphs}});
            const std::string style = event.base_style;
            renders.push_back({{"event", global_event}, {"event_index", event.event.input_index},
                               {"file", file.ass.path}, {"layer", event.event.layer}, {"style", style},
                               {"effective_style", event.effective_style},
                               {"x0", result.bounds.x0}, {"y0", result.bounds.y0}, {"x1", result.bounds.x1},
                               {"y1", result.bounds.y1}, {"w", result.bounds.width()},
                               {"h", result.bounds.height()}, {"glyphs", result.glyphs},
                               {"crc32", crc_hex(result.crc32)}, {"rendered", result.rendered},
                               {"margin_violation", violation}, {"segments", segments}});
            ++global_event;
        }
    }

    for (const auto &diagnostic : extra_diagnostics) {
        auto &target = diagnostic.severity == "error" ? errors : warnings;
        target.push_back(diagnostic_json(diagnostic));
    }
    const int warning_count = static_cast<int>(warnings.size());
    const int error_count = static_cast<int>(errors.size());
    json inline_json = json::object();
    for (const auto &item : inline_counts) inline_json[item.first] = item.second;
    json unknown_json = json::object();
    for (const auto &item : unknown_counts) unknown_json[item.first] = item.second;

    report["summary"] = {{"files", files.size()}, {"events", total_events}, {"styles", total_styles},
                         {"warnings", warning_count}, {"errors", error_count},
                         {"margin_violations", margin_violations}, {"fatal_files", fatal_files},
                         {"render_width", options.width}, {"render_height", options.height},
                         {"safe_x", options.safe_x}, {"safe_y", options.safe_y},
                         {"elapsed_ms", elapsed_ms}};
    report["files"] = std::move(file_rows);
    report["style_chains"] = std::move(chains);
    report["inline_tag_counts"] = std::move(inline_json);
    report["unknown_tag_counts"] = std::move(unknown_json);
    report["render"] = std::move(renders);
    report["margin_violations"] = margin_violations;
    report["warnings"] = std::move(warnings);
    report["errors"] = std::move(errors);
    return report;
}

std::string text_report(const json &report) {
    const auto &summary = report.at("summary");
    std::ostringstream out;
    out << "files=" << summary.at("files") << " events=" << summary.at("events")
        << " styles=" << summary.at("styles") << " warnings=" << summary.at("warnings")
        << " errors=" << summary.at("errors") << " margin_violations=" << summary.at("margin_violations")
        << " elapsed_ms=" << summary.at("elapsed_ms") << '\n';
    for (const auto &chain : report.at("style_chains")) {
        out << "style " << chain.at("file").get<std::string>() << ':' << chain.at("name").get<std::string>()
            << " chain=";
        bool first = true;
        for (const auto &part : chain.at("chain")) {
            if (!first) out << " -> ";
            out << part.get<std::string>();
            first = false;
        }
        out << " font_size=" << chain.at("font_size") << " outline=" << chain.at("outline")
            << " valid=" << (chain.at("valid").get<bool>() ? "true" : "false") << '\n';
    }
    out << "inline_tags";
    for (const auto &item : report.at("inline_tag_counts").items())
        out << ' ' << item.key() << '=' << item.value();
    out << '\n';
    for (const auto &render : report.at("render")) {
        out << "render event=" << render.at("event") << " file=" << render.at("file").get<std::string>()
            << " style=" << render.at("style").get<std::string>() << " x0=" << render.at("x0")
            << " y0=" << render.at("y0") << " x1=" << render.at("x1") << " y1=" << render.at("y1")
            << " w=" << render.at("w") << " h=" << render.at("h") << " glyphs=" << render.at("glyphs")
            << " crc32=" << render.at("crc32").get<std::string>();
        if (render.at("margin_violation").get<bool>()) out << " margin=VIOLATION";
        out << '\n';
    }
    for (const auto &warning : report.at("warnings")) {
        out << "warning " << warning.at("file").get<std::string>() << ':' << warning.at("line")
            << " [" << warning.at("code").get<std::string>() << "] "
            << warning.at("message").get<std::string>() << '\n';
    }
    for (const auto &error : report.at("errors")) {
        out << "error " << error.at("file").get<std::string>() << ':' << error.at("line") << " ["
            << error.at("code").get<std::string>() << "] " << error.at("message").get<std::string>() << '\n';
    }
    return out.str();
}

int report_exit_code(const json &report) {
    if (report.at("summary").at("fatal_files").get<int>() > 0) return 3;
    if (report.at("summary").at("margin_violations").get<int>() > 0) return 1;
    if (report.at("summary").at("warnings").get<int>() > 0) return 1;
    if (report.at("summary").at("errors").get<int>() > 0) return 1;
    return 0;
}

}  // namespace subqc
