#include "access_unit.h"
#include "gop_index.h"
#include "nal_split.h"
#include "param_sets.h"
#include "picture_order.h"
#include "report.h"
#include "slice_header.h"
#include "stats.h"
#include "selftest.h"

#include <fstream>
#include <iostream>
#include <limits>
#include <map>
#include <sstream>
#include <string>
#include <vector>

namespace {

struct Options {
    std::string command;
    std::string executable;
    std::string input;
    std::string json_path;
    bool recovery_point = false;
    bool strict = false;
    std::size_t min_gop = 0;
};

enum class AnalyzeStatus { ok, input_error, structure_error };

void usage() {
    std::cout << "usage:\n"
              << "  h264_audit scan <file.h264> [--json <path>] [--strict]\n"
              << "  h264_audit gops <file.h264> [--recovery-point] [--min-gop <frames>] [--json <path>] [--strict]\n"
              << "  h264_audit selftest [--fixtures <dir>]\n";
}

bool parse_size(const std::string& text, std::size_t& value) {
    if (text.empty() || text[0] == '-') return false;
    unsigned long long parsed = 0;
    std::size_t consumed = 0;
    try {
        parsed = std::stoull(text, &consumed);
    } catch (...) {
        return false;
    }
    if (consumed != text.size() ||
        parsed > static_cast<unsigned long long>(std::numeric_limits<std::size_t>::max())) {
        return false;
    }
    value = static_cast<std::size_t>(parsed);
    return true;
}

bool parse_options(int argc, char** argv, Options& options, std::string& error) {
    if (argc < 2) {
        error = "missing subcommand";
        return false;
    }
    options.command = argv[1];
    if (options.command == "--help" || options.command == "-h") {
        return true;
    }
    if (options.command != "scan" && options.command != "gops" && options.command != "selftest") {
        error = "unknown subcommand: " + options.command;
        return false;
    }
    for (int i = 2; i < argc; ++i) {
        const std::string arg = argv[i];
        if (arg == "--json") {
            if (++i >= argc) { error = "--json requires a path"; return false; }
            options.json_path = argv[i];
        } else if (arg == "--recovery-point") {
            options.recovery_point = true;
        } else if (arg == "--strict") {
            options.strict = true;
        } else if (arg == "--min-gop") {
            if (++i >= argc || !parse_size(argv[i], options.min_gop)) {
                error = "--min-gop requires a non-negative integer";
                return false;
            }
        } else if (arg == "--fixtures") {
            if (++i >= argc) { error = "--fixtures requires a directory"; return false; }
            options.input = argv[i];
        } else if (!arg.empty() && arg[0] == '-') {
            error = "unknown option: " + arg;
            return false;
        } else if (options.command == "selftest") {
            options.input = arg;
        } else if (options.input.empty()) {
            options.input = arg;
        } else {
            error = "unexpected positional argument: " + arg;
            return false;
        }
    }
    if (options.command != "selftest" && options.input.empty()) {
        error = "missing input file";
        return false;
    }
    return true;
}

bool is_truncation_message(const std::string& message) {
    return message.find("unexpected end of RBSP") != std::string::npos ||
           message.find("truncated") != std::string::npos;
}

const h264::SpsInfo* find_sps(const h264::ParamSets& params, const h264::SliceInfo& slice) {
    const auto pps = params.pps.find(slice.pps_id);
    if (pps == params.pps.end()) return nullptr;
    const auto sps = params.sps.find(pps->second.sps_id);
    return sps == params.sps.end() ? nullptr : &sps->second;
}

AnalyzeStatus analyze_stream(const Options& options, h264::AuditReport& report,
                                   std::string& error) {
    std::vector<h264::NalUnit> nals;
    std::uint64_t file_size = 0;
    if (!h264::scan_nal_units(options.input, nals, file_size, error)) {
        return AnalyzeStatus::input_error;
    }
    std::ifstream file(options.input, std::ios::binary);
    if (!file) {
        error = "cannot open input file: " + options.input;
        return AnalyzeStatus::input_error;
    }
    h264::ParamSets params;
    std::vector<h264::SliceInfo> slices;
    std::map<std::size_t, h264::RecoveryPoint> recovery_points;
    std::vector<h264::Warning> warnings;
    std::vector<std::uint8_t> payload;
    for (const h264::NalUnit& nal : nals) {
        if (nal.at_eof && nal.size == 0) {
            warnings.push_back({"warning", nal.offset,
                                "truncated NAL: start code has no header byte"});
            continue;
        }
        if (nal.type != 6 && nal.type != 7 && nal.type != 8 && nal.type != 1 && nal.type != 5) {
            continue;
        }
        if (!h264::read_nal_payload(file, nal, payload, error)) {
            return AnalyzeStatus::input_error;
        }
        std::string parse_error;
        if (nal.type == 7) {
            h264::SpsInfo sps;
            if (!h264::parse_sps(payload.data(), payload.size(), sps, parse_error)) {
                if (!(nal.at_eof && is_truncation_message(parse_error))) {
                    error = "SPS at NAL offset " + std::to_string(nal.offset) + ": " + parse_error;
                    return AnalyzeStatus::structure_error;
                }
                warnings.push_back({"warning", nal.offset, "truncated SPS: " + parse_error});
            } else {
                params.sps[sps.id] = sps;
            }
        } else if (nal.type == 8) {
            h264::PpsInfo pps;
            if (!h264::parse_pps(payload.data(), payload.size(), pps, parse_error)) {
                if (!(nal.at_eof && is_truncation_message(parse_error))) {
                    error = "PPS at NAL offset " + std::to_string(nal.offset) + ": " + parse_error;
                    return AnalyzeStatus::structure_error;
                }
                warnings.push_back({"warning", nal.offset, "truncated PPS: " + parse_error});
            } else {
                params.pps[pps.id] = pps;
            }
        } else if (nal.type == 6) {
            h264::RecoveryPoint recovery;
            if (!h264::parse_recovery_point(payload.data(), payload.size(), recovery, parse_error)) {
                if (nal.at_eof && is_truncation_message(parse_error)) {
                    warnings.push_back({"warning", nal.offset, "truncated SEI: " + parse_error});
                } else {
                    error = "SEI at NAL offset " + std::to_string(nal.offset) + ": " + parse_error;
                    return AnalyzeStatus::structure_error;
                }
            } else if (recovery.present) {
                recovery_points[nal.index] = recovery;
            }
        } else {
            h264::SliceInfo slice;
            if (!h264::parse_slice_header(nal, payload.data(), payload.size(), params, slice, parse_error)) {
                if (nal.at_eof && is_truncation_message(parse_error)) {
                    warnings.push_back({"warning", nal.offset, "truncated slice: " + parse_error});
                    continue;
                }
                error = "slice at NAL offset " + std::to_string(nal.offset) + ": " + parse_error;
                return AnalyzeStatus::structure_error;
            }
            slices.push_back(slice);
        }
    }
    if (slices.empty()) {
        error = "no complete coded slices found";
        return AnalyzeStatus::structure_error;
    }
    const h264::SliceInfo& first_slice = slices.front();
    const auto first_pps = params.pps.find(first_slice.pps_id);
    if (first_pps == params.pps.end()) {
        error = "first slice references an unknown PPS";
        return AnalyzeStatus::structure_error;
    }
    const auto first_sps = params.sps.find(first_pps->second.sps_id);
    if (first_sps == params.sps.end()) {
        error = "first PPS references an unknown SPS";
        return AnalyzeStatus::structure_error;
    }

    std::vector<h264::AccessUnit> aus;
    if (!h264::build_access_units(nals, slices, recovery_points, file_size, aus, error)) {
        return AnalyzeStatus::structure_error;
    }
    h264::PictureOrderCounter poc_counter;
    std::uint64_t previous_order = 0;
    bool have_previous_order = false;
    for (h264::AccessUnit& au : aus) {
        const h264::SpsInfo* sps = find_sps(params, slices[au.first_slice]);
        if (sps == nullptr) {
            error = "access unit at offset " + std::to_string(au.start_offset) + " has no SPS";
            return AnalyzeStatus::structure_error;
        }
        h264::PocInfo poc;
        std::string poc_error;
        if (!poc_counter.compute(slices[au.first_slice], *sps, poc, poc_error)) {
            error = "POC at NAL offset " + std::to_string(au.start_offset) + ": " + poc_error;
            return AnalyzeStatus::structure_error;
        }
        if (au.idr && poc.poc != 0) {
            error = "IDR POC reset is not zero at NAL offset " + std::to_string(au.start_offset);
            return AnalyzeStatus::structure_error;
        }
        if (!au.idr && have_previous_order && poc.frame_num_unwrapped < previous_order) {
            error = "non-monotonic frame order at NAL offset " + std::to_string(au.start_offset);
            return AnalyzeStatus::structure_error;
        }
        previous_order = poc.frame_num_unwrapped;
        have_previous_order = true;
        au.frame_num_unwrapped = poc.frame_num_unwrapped;
        au.poc = poc.poc;
    }
    std::vector<h264::AccessUnit> frames;
    if (!h264::merge_field_pairs(aus, frames, error)) {
        return AnalyzeStatus::structure_error;
    }
    std::vector<h264::GopInfo> gops;
    if (!h264::build_gops(nals, frames, file_size, options.recovery_point,
                          options.min_gop, gops, error)) {
        error = "cannot build GOP index: " + error;
        return AnalyzeStatus::structure_error;
    }
    if (options.strict) {
        if (!frames.front().idr) {
            warnings.push_back({"warning", frames.front().start_offset,
                                "strict: stream does not start with an IDR"});
        }
        for (std::size_t i = 1; i < frames.size(); ++i) {
            if (frames[i].idr) continue;
            const std::uint64_t expected = frames[i - 1].frame_num_unwrapped + 1;
            if (frames[i].frame_num_unwrapped != expected) {
                warnings.push_back({"warning", frames[i].start_offset,
                                    "strict: frame_num gap or non-monotonic frame order"});
            }
        }
        std::size_t indexed_frames = 0;
        for (const h264::GopInfo& gop : gops) indexed_frames += gop.frame_count;
        if (indexed_frames != frames.size()) {
            warnings.push_back({"warning", 0, "strict: GOP frame count does not match stream"});
        }
    }
    report = h264::AuditReport{};
    report.input_path = options.input;
    report.sps = first_sps->second;
    report.pps = first_pps->second;
    report.gops = std::move(gops);
    report.warnings = std::move(warnings);
    report.stats = h264::compute_stats(aus, frames, report.gops, report.warnings);
    return AnalyzeStatus::ok;
}

int run_command(const Options& options) {
    if (options.command == "selftest") {
        const std::string fixtures = options.input.empty() ? "fixtures" : options.input;
        return h264::run_selftest(options.executable, fixtures);
    }
    h264::AuditReport report;
    std::string error;
    const AnalyzeStatus status = analyze_stream(options, report, error);
    if (status == AnalyzeStatus::input_error) {
        std::cerr << "error: " << error << '\n';
        return 1;
    }
    if (status == AnalyzeStatus::structure_error) {
        std::cerr << "structure error: " << error << '\n';
        return 2;
    }
    h264::print_terminal(report, true, true);
    if (!options.json_path.empty()) {
        std::string json_error;
        if (!h264::write_json(options.json_path, report, json_error)) {
            std::cerr << "error: " << json_error << '\n';
            return 1;
        }
    }
    if (!report.warnings.empty()) {
        std::cerr << "warning: stream completed with " << report.warnings.size()
                  << " warning(s)\n";
        return 2;
    }
    return 0;
}

}  // namespace

int main(int argc, char** argv) {
    Options options;
    options.executable = argv[0];
    std::string error;
    if (!parse_options(argc, argv, options, error)) {
        std::cerr << "error: " << error << "\n\n";
        usage();
        return 1;
    }
    if (options.command == "--help" || options.command == "-h") {
        usage();
        return 0;
    }
    return run_command(options);
}
