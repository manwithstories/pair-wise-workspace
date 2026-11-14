#include <chrono>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <optional>
#include <string>
#include <vector>

#include "nlohmann/json.hpp"
#include "subqc/ass_reader.hpp"
#include "subqc/event_model.hpp"
#include "subqc/fixture_gen.hpp"
#include "subqc/metrics.hpp"
#include "subqc/model.hpp"
#include "subqc/render_probe.hpp"
#include "subqc/style_chain.hpp"

namespace {

using namespace subqc;

void usage(std::ostream &out) {
    out << "usage:\n"
        << "  subqc gen-fixtures --out DIR [--events N] [--seed N]\n"
        << "  subqc scan --input PATH --width N --height N --font-dir DIR\n"
        << "             [--json FILE] [--text] [--safe-x N] [--safe-y N]\n";
}

bool parse_int(const std::string &text, int &value) {
    try {
        std::size_t used = 0;
        const long long parsed = std::stoll(text, &used, 10);
        if (used != text.size()) return false;
        value = static_cast<int>(parsed);
        return true;
    } catch (...) {
        return false;
    }
}

bool parse_u64(const std::string &text, std::uint64_t &value) {
    try {
        std::size_t used = 0;
        value = std::stoull(text, &used, 10);
        return used == text.size();
    } catch (...) {
        return false;
    }
}

int run_gen_fixtures(int argc, char **argv) {
    std::string out;
    int events = 240;
    std::uint64_t seed = 20261114;
    for (int i = 2; i < argc; ++i) {
        const std::string arg = argv[i];
        if (arg == "--out" && i + 1 < argc) out = argv[++i];
        else if (arg == "--events" && i + 1 < argc && parse_int(argv[++i], events)) {}
        else if (arg == "--seed" && i + 1 < argc && parse_u64(argv[++i], seed)) {}
        else {
            std::cerr << "subqc: invalid gen-fixtures argument: " << arg << '\n';
            usage(std::cerr);
            return 2;
        }
    }
    if (out.empty() || events <= 0) {
        std::cerr << "subqc: --out and a positive --events value are required\n";
        return 2;
    }
    FixtureSummary summary;
    std::string error;
    if (!generate_fixtures(out, events, seed, summary, error)) {
        std::cerr << "subqc: " << error << '\n';
        return 1;
    }
    std::cout << "generated files=" << summary.files << " events=" << summary.events
              << " styles=" << summary.styles << " out=" << out << '\n';
    return 0;
}

int run_scan(int argc, char **argv) {
    std::string input;
    int width = 1920;
    int height = 1080;
    std::string font_dir;
    std::string json_path;
    bool text = false;
    int safe_x_arg = -1;
    int safe_y_arg = -1;
    for (int i = 2; i < argc; ++i) {
        const std::string arg = argv[i];
        if (arg == "--input" && i + 1 < argc) input = argv[++i];
        else if (arg == "--width" && i + 1 < argc && parse_int(argv[++i], width)) {}
        else if (arg == "--height" && i + 1 < argc && parse_int(argv[++i], height)) {}
        else if (arg == "--font-dir" && i + 1 < argc) font_dir = argv[++i];
        else if (arg == "--json" && i + 1 < argc) json_path = argv[++i];
        else if (arg == "--text") text = true;
        else if (arg == "--safe-x" && i + 1 < argc && parse_int(argv[++i], safe_x_arg)) {}
        else if (arg == "--safe-y" && i + 1 < argc && parse_int(argv[++i], safe_y_arg)) {}
        else {
            std::cerr << "subqc: invalid scan argument: " << arg << '\n';
            usage(std::cerr);
            return 2;
        }
    }
    if (input.empty() || font_dir.empty() || width <= 0 || height <= 0) {
        std::cerr << "subqc: --input, --font-dir and positive dimensions are required\n";
        return 2;
    }
    std::error_code ec;
    if (!std::filesystem::exists(input, ec)) {
        std::cerr << "subqc: input does not exist: " << input << '\n';
        return 2;
    }
    if (!std::filesystem::is_directory(font_dir, ec)) {
        std::cerr << "subqc: font directory does not exist: " << font_dir << '\n';
        return 2;
    }
    const auto paths = collect_ass_files(input);
    if (paths.empty()) {
        std::cerr << "subqc: no .ass/.ssa files found under " << input << '\n';
        return 3;
    }
    RenderOptions options;
    options.width = width;
    options.height = height;
    options.font_dir = font_dir;
    options.safe_x = safe_x_arg >= 0 ? safe_x_arg : width * 5 / 100;
    options.safe_y = safe_y_arg >= 0 ? safe_y_arg : height * 5 / 100;

    const auto started = std::chrono::steady_clock::now();
    std::vector<AnalyzedFile> files;
    files.reserve(paths.size());
    for (const auto &path : paths) {
        AnalyzedFile analyzed;
        analyzed.load_failed = !read_ass_file(path, analyzed.ass);
        if (!analyzed.load_failed) {
            analyzed.catalog = resolve_styles(analyzed.ass, width, height);
            analyzed.ass.diagnostics.insert(analyzed.ass.diagnostics.end(), analyzed.catalog.diagnostics.begin(),
                                            analyzed.catalog.diagnostics.end());
            build_event_model(analyzed);
        }
        files.push_back(std::move(analyzed));
    }

    std::vector<Diagnostic> extra_diagnostics;
    auto probe = RenderProbe::create(options, extra_diagnostics);
    if (probe) {
        for (auto &file : files) {
            if (file.ass.fatal || file.load_failed) continue;
            if (!probe->render_file(file, options)) file.load_failed = true;
        }
    } else {
        for (auto &file : files) {
            if (!file.load_failed) file.load_failed = true;
        }
    }
    const auto finished = std::chrono::steady_clock::now();
    const auto elapsed = std::chrono::duration_cast<std::chrono::milliseconds>(finished - started).count();
    const auto report = build_report(files, options, elapsed, extra_diagnostics);

    if (!json_path.empty()) {
        const std::filesystem::path output_path(json_path);
        if (output_path.has_parent_path()) std::filesystem::create_directories(output_path.parent_path(), ec);
        std::ofstream output(output_path, std::ios::binary);
        if (!output) {
            std::cerr << "subqc: cannot write JSON report: " << json_path << '\n';
            return 2;
        }
        output << report.dump(2) << '\n';
    }
    if (text) std::cout << text_report(report);
    else if (json_path.empty()) std::cout << report.dump(2) << '\n';
    return report_exit_code(report);
}

}  // namespace

int main(int argc, char **argv) {
    if (argc >= 2) {
        const std::string command = argv[1];
        if (command == "--help" || command == "-h") {
            usage(std::cout);
            return 0;
        }
        if (command == "gen-fixtures") return run_gen_fixtures(argc, argv);
        if (command == "scan") return run_scan(argc, argv);
        std::cerr << "subqc: unknown command: " << command << '\n';
    }
    usage(std::cerr);
    return 2;
}
