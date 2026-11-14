#include "compat_check.h"
#include "error.h"
#include "json.h"
#include "repacker.h"
#include "report_writer.h"
#include "segment_index.h"
#include "util.h"

#include <iostream>
#include <map>
#include <string>

namespace {

using mp4::ExitCode;

const char* usage() {
    return "usage:\n"
           "  mp4pack index <directory> --out <index.json>\n"
           "  mp4pack validate --index <index.json> --init <init.mp4> --out <validate.json>\n"
           "  mp4pack repack --index <index.json> --init <init.mp4> --out <repacked.mp4>\n"
           "  mp4pack report --index <index.json> --repacked <repacked.mp4> [--out <report.json>]\n";
}

std::map<std::string, std::string> parse_options(int argc, char** argv, int start) {
    std::map<std::string, std::string> options;
    for (int i = start; i < argc; ++i) {
        const std::string arg = argv[i];
        if (arg.rfind("--", 0) != 0 || i + 1 >= argc) {
            mp4::fail(ExitCode::FormatError, "invalid argument: " + arg);
        }
        options[arg] = argv[++i];
    }
    return options;
}

const std::string& required_option(const std::map<std::string, std::string>& options,
                                   const std::string& name) {
    const auto it = options.find(name);
    if (it == options.end() || it->second.empty()) {
        mp4::fail(ExitCode::FormatError, "missing required option " + name);
    }
    return it->second;
}

std::string optional_option(const std::map<std::string, std::string>& options,
                            const std::string& name) {
    const auto it = options.find(name);
    return it == options.end() ? std::string() : it->second;
}

bool index_has_segment_issue(const mp4::SegmentIndex& index) {
    if (!index.missing_sequences.empty() || index.duplicate_count > 0 || index.out_of_order_count > 0) {
        return true;
    }
    for (const auto& track : index.tracks) {
        if (track.unresolved_segments > 0) return true;
    }
    return false;
}

std::string number_list(const std::vector<uint32_t>& values) {
    std::string out = "[";
    for (size_t i = 0; i < values.size(); ++i) {
        if (i != 0) out += ",";
        out += std::to_string(values[i]);
    }
    return out + "]";
}

mp4::SegmentIndex load_index(const std::string& path) {
    return mp4::index_from_json(mp4::Json::parse(mp4::read_text_file(path)));
}

int run_index(int argc, char** argv) {
    if (argc != 5) mp4::fail(ExitCode::FormatError, "index expects <directory> --out <file>");
    const std::string directory = argv[2];
    const auto options = parse_options(argc, argv, 3);
    const std::string& output = required_option(options, "--out");
    const mp4::SegmentIndex index = mp4::build_index(directory);
    mp4::write_text_file_atomic(output, mp4::index_to_json(index).dump());
    std::cout << "segments_found=" << index.segments_found << '\n'
              << "segments_after_repack=" << index.segments_after_repack << '\n'
              << "missing_sequences=" << number_list(index.missing_sequences) << '\n'
              << "seq_contiguous=" << (index.seq_contiguous ? "true" : "false") << '\n';
    return index_has_segment_issue(index) ? 2 : 0;
}

int run_validate(int argc, char** argv) {
    const auto options = parse_options(argc, argv, 2);
    const auto& index_path = required_option(options, "--index");
    const auto& init_path = required_option(options, "--init");
    const auto& output = required_option(options, "--out");
    const mp4::SegmentIndex index = load_index(index_path);
    const mp4::InitInfo candidate = mp4::parse_init_file(init_path);
    const mp4::ValidationResult result = mp4::validate_init(index, candidate);
    mp4::write_text_file_atomic(output, mp4::validation_to_json(result, index).dump());
    for (const auto& check : result.checks) {
        std::cout << (check.ok ? "ok" : "fail") << " " << check.name << ": " << check.detail << '\n';
    }
    std::cout << "compatible=" << (result.init_compatible ? "true" : "false") << '\n'
              << "verdict=" << result.verdict << '\n';
    if (!result.init_compatible) return 3;
    return result.segment_ok ? 0 : 2;
}

int run_repack(int argc, char** argv) {
    const auto options = parse_options(argc, argv, 2);
    const auto& index_path = required_option(options, "--index");
    const auto& init_path = required_option(options, "--init");
    const auto& output = required_option(options, "--out");
    const mp4::SegmentIndex index = load_index(index_path);
    const mp4::InitInfo candidate = mp4::parse_init_file(init_path);
    const mp4::ValidationResult validation = mp4::validate_init(index, candidate);
    if (!validation.init_compatible) {
        std::cerr << "initialization segment is incompatible\n";
        return 3;
    }
    const mp4::RepackResult result = mp4::repack_index(index, init_path, output);
    std::cout << "repacked=" << result.segments_written << '\n'
              << "output=" << output << '\n';
    return result.source_has_segment_issue ? 2 : 0;
}

int run_report(int argc, char** argv) {
    const auto options = parse_options(argc, argv, 2);
    const auto& index_path = required_option(options, "--index");
    const auto& repacked_path = required_option(options, "--repacked");
    const mp4::SegmentIndex index = load_index(index_path);
    const mp4::ReportResult report = mp4::analyze_repacked(index, repacked_path);
    std::cout << mp4::report_summary(report);
    const std::string output = optional_option(options, "--out");
    if (!output.empty()) {
        mp4::write_text_file_atomic(output, mp4::report_to_json(report).dump());
    }
    if (report.verdict == "ok") return 0;
    if (report.verdict == "repack_error") return 4;
    return 2;
}

}  // namespace

int main(int argc, char** argv) {
    if (argc < 2 || std::string(argv[1]) == "--help" || std::string(argv[1]) == "-h") {
        std::cout << usage();
        return 0;
    }
    try {
        const std::string command = argv[1];
        if (command == "index") return run_index(argc, argv);
        if (command == "validate") return run_validate(argc, argv);
        if (command == "repack") return run_repack(argc, argv);
        if (command == "report") return run_report(argc, argv);
        std::cerr << "unknown command: " << command << '\n' << usage();
        return static_cast<int>(ExitCode::FormatError);
    } catch (const mp4::Error& error) {
        std::cerr << "error: " << error.what() << '\n';
        return static_cast<int>(error.code());
    } catch (const std::exception& error) {
        std::cerr << "error: " << error.what() << '\n';
        return static_cast<int>(ExitCode::FormatError);
    }
}
