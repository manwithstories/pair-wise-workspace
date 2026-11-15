#include "analyzer.h"
#include "fixture_gen.h"
#include "json_writer.h"

#include <algorithm>
#include <cstdint>
#include <filesystem>
#include <iostream>
#include <limits>
#include <string>
#include <system_error>
#include <vector>

namespace {

constexpr int kExitSuccess = 0;
constexpr int kExitStrictWarning = 1;
constexpr int kExitCorrupt = 2;
constexpr int kExitNoSps = 3;
constexpr int kExitSynthArgument = 4;

void printUsage(std::ostream& output) {
    output << "Usage:\n"
           << "  h264scan synth --out <file> [--gops N] [--slices N]\n"
           << "                 [--interlaced pair|none] [--seed N]\n"
           << "  h264scan analyze <file|dir>... [--recursive] [--json <file>]\n"
           << "                   [--csv <file>] [--strict] [--verbose]\n"
           << "Exit codes: 0 ok, 1 strict warning, 2 corrupt/I-O, 3 no SPS, "
              "4 bad synth arguments\n";
}

bool parseUint64(const std::string& text, uint64_t& value) {
    if (text.empty()) {
        return false;
    }
    uint64_t parsed = 0;
    for (char ch : text) {
        if (ch < '0' || ch > '9') {
            return false;
        }
        const uint64_t digit = static_cast<uint64_t>(ch - '0');
        if (parsed > (std::numeric_limits<uint64_t>::max() - digit) / 10U) {
            return false;
        }
        parsed = parsed * 10U + digit;
    }
    value = parsed;
    return true;
}

int runSynth(const std::vector<std::string>& args) {
    h264scan::SynthOptions options;
    bool has_output = false;
    for (size_t i = 0; i < args.size(); ++i) {
        const std::string& arg = args[i];
        if (arg == "--out" && i + 1U < args.size()) {
            options.output_path = args[++i];
            has_output = true;
        } else if (arg == "--gops" && i + 1U < args.size()) {
            uint64_t value = 0;
            if (!parseUint64(args[++i], value) || value > UINT32_MAX) {
                std::cerr << "invalid --gops value\n";
                return kExitSynthArgument;
            }
            options.gops = static_cast<uint32_t>(value);
        } else if (arg == "--slices" && i + 1U < args.size()) {
            uint64_t value = 0;
            if (!parseUint64(args[++i], value) || value > UINT32_MAX) {
                std::cerr << "invalid --slices value\n";
                return kExitSynthArgument;
            }
            options.slices = static_cast<uint32_t>(value);
        } else if (arg == "--interlaced" && i + 1U < args.size()) {
            const std::string value = args[++i];
            if (value == "pair") {
                options.interlaced_pair = true;
            } else if (value == "none") {
                options.interlaced_pair = false;
            } else {
                std::cerr << "--interlaced must be pair or none\n";
                return kExitSynthArgument;
            }
        } else if (arg == "--seed" && i + 1U < args.size()) {
            if (!parseUint64(args[++i], options.seed)) {
                std::cerr << "invalid --seed value\n";
                return kExitSynthArgument;
            }
        } else {
            std::cerr << "unknown or incomplete synth option: " << arg << '\n';
            return kExitSynthArgument;
        }
    }
    if (!has_output) {
        std::cerr << "--out is required\n";
        return kExitSynthArgument;
    }

    std::string error;
    if (!h264scan::validateSynthOptions(options, error)) {
        std::cerr << error << '\n';
        return kExitSynthArgument;
    }
    if (!h264scan::generateFixture(options, error)) {
        std::cerr << error << '\n';
        return kExitCorrupt;
    }
    std::cout << "generated " << options.output_path << " (gops=" << options.gops
              << ", frames/gop=24, slices/frame=" << options.slices
              << ", interlaced=" << (options.interlaced_pair ? "pair" : "none")
              << ")\n";
    return kExitSuccess;
}

bool collectInputs(const std::vector<std::string>& paths, bool recursive,
                   std::vector<std::string>& files, std::string& error) {
    files.clear();
    for (const std::string& path : paths) {
        std::error_code status_error;
        const std::filesystem::file_status status =
            std::filesystem::status(path, status_error);
        if (status_error || !std::filesystem::exists(status)) {
            error = "input does not exist: " + path;
            return false;
        }
        if (std::filesystem::is_regular_file(status)) {
            files.push_back(path);
            continue;
        }
        if (!std::filesystem::is_directory(status)) {
            error = "input is not a regular file or directory: " + path;
            return false;
        }
        if (!recursive) {
            error = "directory input requires --recursive: " + path;
            return false;
        }
        std::error_code iterator_error;
        std::filesystem::recursive_directory_iterator iterator(
            path, std::filesystem::directory_options::skip_permission_denied,
            iterator_error);
        const std::filesystem::recursive_directory_iterator end;
        if (iterator_error) {
            error = "cannot read directory " + path + ": " + iterator_error.message();
            return false;
        }
        for (; iterator != end; iterator.increment(iterator_error)) {
            if (iterator_error) {
                error = "cannot read directory " + path + ": " +
                        iterator_error.message();
                return false;
            }
            if (iterator->is_regular_file(iterator_error) && !iterator_error) {
                files.push_back(iterator->path().string());
            }
            iterator_error.clear();
        }
    }
    std::sort(files.begin(), files.end());
    files.erase(std::unique(files.begin(), files.end()), files.end());
    if (files.empty()) {
        error = "no input files found";
        return false;
    }
    return true;
}

void printReport(const h264scan::FileReport& report, bool verbose) {
    if (report.has_sps) {
        std::cout << report.file << ": " << report.sps.width << 'x'
                  << report.sps.height << " profile=" << report.sps.profile_idc
                  << " level=" << report.sps.level_idc << " gops="
                  << report.totals.gop_count << " frames=" << report.totals.frames
                  << " slices=" << report.totals.slice_count
                  << " fields=" << report.totals.field_pairs
                  << " warnings=" << report.totals.warnings << '\n';
    } else {
        std::cout << report.file << ": no valid SPS, nals=" << report.nal_count
                  << " warnings=" << report.totals.warnings << '\n';
    }

    if (verbose) {
        if (report.has_sps) {
            std::cout << "  sps: chroma=" << report.sps.chroma_format_idc
                      << " frame_mbs_only=" << (report.sps.frame_mbs_only_flag ? 1 : 0)
                      << " log2_max_frame_num_minus4="
                      << report.sps.log2_max_frame_num_minus4
                      << " poc_type=" << report.sps.pic_order_cnt_type << '\n';
        }
        std::cout << "  nals=" << report.nal_count
                  << " sps=" << report.sps_count << " pps=" << report.pps_count
                  << " unknown=" << report.unknown_nal_count
                  << " truncated=" << report.truncated_nal_count << '\n';
        for (const h264scan::GopStats& gop : report.gops) {
            std::cout << "  gop " << gop.index << ": offset=" << gop.start_offset
                      << " bytes=" << gop.byte_length << " frames=" << gop.frames
                      << " I/P/B=" << gop.i_frames << '/' << gop.p_frames << '/'
                      << gop.b_frames << " fields=" << gop.field_pairs
                      << " slices=" << gop.slice_count
                      << " warnings=" << gop.warnings << '\n';
        }
        for (const std::string& message : report.warnings) {
            std::cerr << "warning: " << report.file << ": " << message << '\n';
        }
    }
}

int runAnalyze(const std::vector<std::string>& args) {
    bool recursive = false;
    bool strict = false;
    bool verbose = false;
    std::string json_path;
    std::string csv_path;
    std::vector<std::string> input_paths;

    for (size_t i = 0; i < args.size(); ++i) {
        const std::string& arg = args[i];
        if (arg == "--recursive") {
            recursive = true;
        } else if (arg == "--strict") {
            strict = true;
        } else if (arg == "--verbose") {
            verbose = true;
        } else if (arg == "--json" && i + 1U < args.size()) {
            json_path = args[++i];
        } else if (arg == "--csv" && i + 1U < args.size()) {
            csv_path = args[++i];
        } else if (!arg.empty() && arg.front() == '-') {
            std::cerr << "unknown or incomplete analyze option: " << arg << '\n';
            return kExitCorrupt;
        } else {
            input_paths.push_back(arg);
        }
    }
    if (input_paths.empty()) {
        std::cerr << "analyze requires at least one input path\n";
        return kExitCorrupt;
    }

    std::vector<std::string> files;
    std::string error;
    if (!collectInputs(input_paths, recursive, files, error)) {
        std::cerr << error << '\n';
        return kExitCorrupt;
    }

    std::vector<h264scan::FileReport> reports;
    bool corrupt = false;
    bool missing_sps = false;
    bool strict_warning = false;
    for (const std::string& file : files) {
        h264scan::FileReport report;
        report.file = file;
        const int result = h264scan::analyzeFile(file, strict, report, error);
        if (!error.empty() && result == kExitCorrupt) {
            std::cerr << file << ": " << error << '\n';
        }
        if (result == kExitCorrupt) corrupt = true;
        if (result == kExitNoSps) missing_sps = true;
        if (result == kExitStrictWarning) strict_warning = true;
        printReport(report, verbose);
        reports.push_back(std::move(report));
    }

    if (!json_path.empty() && !h264scan::writeJsonReports(reports, json_path, error)) {
        std::cerr << error << '\n';
        return kExitCorrupt;
    }
    if (!csv_path.empty() && !h264scan::writeCsvReports(reports, csv_path, error)) {
        std::cerr << error << '\n';
        return kExitCorrupt;
    }

    if (corrupt) return kExitCorrupt;
    if (missing_sps) return kExitNoSps;
    if (strict_warning) return kExitStrictWarning;
    return kExitSuccess;
}

}  // namespace

int main(int argc, char** argv) {
    std::vector<std::string> args;
    for (int i = 1; i < argc; ++i) {
        args.emplace_back(argv[i]);
    }
    if (args.empty() || args.front() == "--help" || args.front() == "-h") {
        printUsage(std::cout);
        return args.empty() ? kExitCorrupt : kExitSuccess;
    }

    const std::string command = args.front();
    args.erase(args.begin());
    if (command == "synth") {
        return runSynth(args);
    }
    if (command == "analyze") {
        return runAnalyze(args);
    }
    std::cerr << "unknown command: " << command << '\n';
    printUsage(std::cerr);
    return kExitCorrupt;
}
