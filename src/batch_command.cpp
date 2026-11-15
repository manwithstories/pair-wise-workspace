#include "subtitle_audit/command_utils.hpp"

#include <algorithm>
#include <cctype>
#include <filesystem>
#include <iostream>
#include <set>
#include <string>
#include <vector>

#include "subtitle_audit/ass_doc.hpp"
#include "subtitle_audit/layout_probe.hpp"
#include "subtitle_audit/report.hpp"
#include "subtitle_audit/style_chain.hpp"

namespace audit {

int run_batch_command(int argc, char **argv) {
    const CliOptions options = parse_cli_options(argc, argv, 2);
    std::string error = options.error;
    if (error.empty() &&
        !check_cli_options(options, {"dir", "out", "frame"}, {"dir"}, error)) {}
    if (!error.empty()) { std::cerr << "error: " << error << '\n'; return 2; }
    const int frame = parse_int_option(options, "frame", 1250, 0, 86400000, error);
    if (!error.empty()) { std::cerr << "error: " << error << '\n'; return 2; }
    const std::filesystem::path directory(options.values.at("dir"));
    std::vector<std::filesystem::path> scripts;
    std::error_code ec;
    if (std::filesystem::is_directory(directory, ec)) {
        for (const auto &entry : std::filesystem::directory_iterator(directory, ec)) {
            if (!entry.is_regular_file()) continue;
            std::string extension = entry.path().extension().string();
            std::transform(extension.begin(), extension.end(), extension.begin(),
                           [](unsigned char ch) { return static_cast<char>(std::tolower(ch)); });
            if (extension == ".ass") scripts.push_back(entry.path());
        }
    }
    std::sort(scripts.begin(), scripts.end(), [](const auto &left, const auto &right) {
        return left.filename().string() < right.filename().string();
    });
    if (ec || scripts.empty()) {
        std::cerr << "error: no .ass scripts found in " << directory << '\n';
        return 2;
    }
    std::vector<BatchFileSummary> summaries;
    bool parse_failed = false, render_failed = false;
    for (const auto &path : scripts) {
        BatchFileSummary summary;
        summary.path = path.string();
        const auto doc = parse_ass_file(summary.path);
        print_diagnostics(doc);
        if (!doc.valid()) {
            summary.parse_ok = false;
            summary.error = doc.parse_error;
            parse_failed = true;
            summaries.push_back(std::move(summary));
            continue;
        }
        summary.events = doc.events.size();
        summary.styles = doc.styles.size();
        const auto chains = analyze_style_chains(doc);
        if (!chains.ok()) {
            summary.parse_ok = false;
            summary.error = chains.errors.front().message;
            parse_failed = true;
            summaries.push_back(std::move(summary));
            continue;
        }
        const auto tags = aggregate_tags(doc);
        summary.tags = tags.total;
        summary.unknown = tags.unknown;
        summary.t_nested = tags.t_nested;
        summary.r_switch = tags.r_switch;
        const auto probe = render_probe(doc, frame, "");
        if (!probe.ok) {
            summary.render_ok = false;
            summary.error = probe.error;
            render_failed = true;
        } else {
            summary.visible = probe.visible;
            summary.max_width = probe.max_width;
            summary.max_height = probe.max_height;
            summary.missing_fonts = probe.missing_fonts;
        }
        std::cout << path.filename().string() << " events=" << summary.events
                  << " tags=" << summary.tags << " visible=" << summary.visible
                  << " max=" << summary.max_width << 'x' << summary.max_height << '\n';
        summaries.push_back(std::move(summary));
    }
    const std::string json = batch_report_json(directory.string(), summaries, frame);
    if (options.values.count("out")) {
        if (options.values.at("out") == "-") std::cout << json << '\n';
        else if (!write_text_file(options.values.at("out"), json)) {
            std::cerr << "error: cannot write JSON output\n";
            return 2;
        }
    }
    std::set<std::string> missing;
    for (const auto &summary : summaries)
        missing.insert(summary.missing_fonts.begin(), summary.missing_fonts.end());
    std::cout << "files " << summaries.size() << '\n';
    if (!missing.empty()) {
        std::cout << "missing_fonts";
        for (const auto &font : missing) std::cout << ' ' << font;
        std::cout << '\n';
    }
    if (render_failed) return 4;
    if (parse_failed) return 3;
    if (!missing.empty()) return 5;
    return 0;
}

}  // namespace audit
