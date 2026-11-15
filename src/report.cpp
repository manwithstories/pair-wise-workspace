#include "report.h"

#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <sstream>

namespace h264 {
namespace {

std::string json_string(const std::string& value) {
    std::ostringstream out;
    out << '"';
    for (const unsigned char ch : value) {
        switch (ch) {
            case '"': out << "\\\""; break;
            case '\\': out << "\\\\"; break;
            case '\b': out << "\\b"; break;
            case '\f': out << "\\f"; break;
            case '\n': out << "\\n"; break;
            case '\r': out << "\\r"; break;
            case '\t': out << "\\t"; break;
            default:
                if (ch < 0x20) {
                    out << "\\u" << std::hex << std::setw(4) << std::setfill('0')
                        << static_cast<int>(ch) << std::dec << std::setfill(' ');
                } else {
                    out << static_cast<char>(ch);
                }
        }
    }
    out << '"';
    return out.str();
}

void write_sps(std::ostream& out, const SpsInfo& sps) {
    out << "  \"sps\": {\n";
    out << "    \"id\": " << sps.id << ",\n";
    out << "    \"profile_idc\": " << sps.profile_idc << ",\n";
    out << "    \"constraint_flags\": " << sps.constraint_flags << ",\n";
    out << "    \"level_idc\": " << sps.level_idc << ",\n";
    out << "    \"chroma_format_idc\": " << sps.chroma_format_idc << ",\n";
    out << "    \"bit_depth_luma\": " << sps.bit_depth_luma << ",\n";
    out << "    \"bit_depth_chroma\": " << sps.bit_depth_chroma << ",\n";
    out << "    \"log2_max_frame_num\": " << sps.log2_max_frame_num << ",\n";
    out << "    \"pic_order_cnt_type\": " << sps.pic_order_cnt_type << ",\n";
    out << "    \"log2_max_pic_order_cnt_lsb\": " << sps.log2_max_pic_order_cnt_lsb << ",\n";
    out << "    \"frame_mbs_only_flag\": " << (sps.frame_mbs_only_flag ? "true" : "false") << ",\n";
    out << "    \"mb_adaptive_frame_field_flag\": " << (sps.mb_adaptive_frame_field_flag ? "true" : "false") << ",\n";
    out << "    \"frame_cropping_flag\": " << (sps.frame_cropping_flag ? "true" : "false") << ",\n";
    out << "    \"coded_width\": " << sps.coded_width << ",\n";
    out << "    \"coded_height\": " << sps.coded_height << ",\n";
    out << "    \"width\": " << sps.width << ",\n";
    out << "    \"height\": " << sps.height << ",\n";
    out << "    \"vui\": {\"timing_info_present_flag\": "
        << (sps.timing_info_present_flag ? "true" : "false")
        << ", \"num_units_in_tick\": " << sps.num_units_in_tick
        << ", \"time_scale\": " << sps.time_scale
        << ", \"fixed_frame_rate_flag\": " << (sps.fixed_frame_rate_flag ? "true" : "false")
        << "}\n";
    out << "  },\n";
}

void write_pps(std::ostream& out, const PpsInfo& pps) {
    out << "  \"pps\": {\n";
    out << "    \"pps_id\": " << pps.id << ",\n";
    out << "    \"sps_id\": " << pps.sps_id << ",\n";
    out << "    \"entropy_coding_mode_flag\": " << (pps.entropy_coding_mode_flag ? "true" : "false") << ",\n";
    out << "    \"bottom_field_pic_order_in_frame_present_flag\": "
        << (pps.bottom_field_pic_order_in_frame_present_flag ? "true" : "false") << ",\n";
    out << "    \"redundant_pic_cnt_present_flag\": "
        << (pps.redundant_pic_cnt_present_flag ? "true" : "false") << ",\n";
    out << "    \"num_slice_groups_minus1\": " << pps.num_slice_groups_minus1 << "\n";
    out << "  },\n";
}

void write_frames(std::ostream& out, const StatsSummary& stats) {
    out << "  \"frames\": {\n";
    out << "    \"total\": " << stats.total_frames << ",\n";
    out << "    \"I\": " << stats.i_frames << ",\n";
    out << "    \"P\": " << stats.p_frames << ",\n";
    out << "    \"B\": " << stats.b_frames << ",\n";
    out << "    \"other\": " << stats.other_frames << ",\n";
    out << "    \"slices_per_au_max\": " << stats.slices_per_au_max << ",\n";
    out << "    \"total_slices\": " << stats.total_slices << ",\n";
    out << "    \"total_bytes\": " << stats.total_bytes << ",\n";
    out << "    \"average_gop_bytes\": " << std::fixed << std::setprecision(2)
        << stats.average_gop_bytes << ",\n";
    out << "    \"slices_per_au_histogram\": {";
    bool first = true;
    for (const auto& item : stats.slices_histogram) {
        if (!first) out << ", ";
        out << '"' << item.first << "\": " << item.second;
        first = false;
    }
    out << "},\n";
    out << "    \"warning_counts\": {";
    first = true;
    for (const auto& item : stats.warning_counts) {
        if (!first) out << ", ";
        out << json_string(item.first) << ": " << item.second;
        first = false;
    }
    out << "}\n";
    out << "  },\n";
}

void write_gops(std::ostream& out, const std::vector<GopInfo>& gops) {
    out << "  \"gops\": [\n";
    for (std::size_t i = 0; i < gops.size(); ++i) {
        const GopInfo& gop = gops[i];
        out << "    {\"index\": " << gop.index
            << ", \"start_offset\": " << gop.start_offset
            << ", \"end_offset\": " << gop.end_offset
            << ", \"byte_length\": " << gop.byte_length
            << ", \"first_nal\": " << gop.first_nal
            << ", \"last_nal\": " << gop.last_nal
            << ", \"frames\": " << gop.frame_count
            << ", \"starts_with_recovery_point\": "
            << (gop.starts_with_recovery_point ? "true" : "false")
            << ", \"frame_types\": " << json_string(gop.frame_types) << '}';
        out << (i + 1 < gops.size() ? ",\n" : "\n");
    }
    out << "  ],\n";
}

void write_warnings(std::ostream& out, const std::vector<Warning>& warnings) {
    out << "  \"warnings\": [\n";
    for (std::size_t i = 0; i < warnings.size(); ++i) {
        const Warning& warning = warnings[i];
        out << "    {\"level\": " << json_string(warning.level)
            << ", \"offset\": " << warning.offset
            << ", \"message\": " << json_string(warning.message) << '}';
        out << (i + 1 < warnings.size() ? ",\n" : "\n");
    }
    out << "  ]\n";
}

}  // namespace

void print_terminal(const AuditReport& report, bool show_summary, bool show_gops) {
    if (show_summary) {
        std::cout << "Input: " << report.input_path << '\n';
        std::cout << "SPS profile_idc=" << report.sps.profile_idc
                  << " level_idc=" << report.sps.level_idc
                  << " chroma_format_idc=" << report.sps.chroma_format_idc
                  << " bit_depth_luma=" << report.sps.bit_depth_luma
                  << " pic_order_cnt_type=" << report.sps.pic_order_cnt_type
                  << " " << report.sps.width << "x" << report.sps.height
                  << " bit_depth_chroma=" << report.sps.bit_depth_chroma << '\n';
        std::cout << "SPS flags frame_mbs_only_flag=" << report.sps.frame_mbs_only_flag
                  << " mb_adaptive_frame_field_flag=" << report.sps.mb_adaptive_frame_field_flag
                  << " coded=" << report.sps.coded_width << "x" << report.sps.coded_height;
        if (report.sps.timing_info_present_flag) {
            const double fps = report.sps.num_units_in_tick == 0
                                   ? 0.0
                                   : static_cast<double>(report.sps.time_scale) /
                                         (2.0 * static_cast<double>(report.sps.num_units_in_tick));
            std::cout << " fps=" << std::fixed << std::setprecision(2) << fps;
        }
        std::cout << '\n';
        std::cout << "PPS pps_id=" << report.pps.id << " sps_id=" << report.pps.sps_id
                  << " entropy_coding_mode_flag=" << report.pps.entropy_coding_mode_flag
                  << " bottom_field_pic_order_in_frame_present_flag="
                  << report.pps.bottom_field_pic_order_in_frame_present_flag << '\n';
        std::cout << "Frames: frames=" << report.stats.total_frames
                  << " I=" << report.stats.i_frames
                  << " P=" << report.stats.p_frames
                  << " B=" << report.stats.b_frames
                  << " slices_per_au_max=" << report.stats.slices_per_au_max
                  << " other=" << report.stats.other_frames
                  << " total_slices=" << report.stats.total_slices
                  << " average_gop_bytes=" << std::fixed << std::setprecision(2)
                  << report.stats.average_gop_bytes << '\n';
        std::cout << "Slice histogram:";
        for (const auto& item : report.stats.slices_histogram) {
            std::cout << ' ' << item.first << "->" << item.second;
        }
        std::cout << '\n';
    }
    if (show_gops) {
        std::cout << "GOPs: count=" << report.gops.size() << '\n';
        std::cout << std::left << std::setw(5) << "gop"
                  << std::right << std::setw(14) << "start_offset"
                  << std::setw(14) << "byte_length"
                  << std::setw(8) << "frames"
                  << "  " << std::setw(8) << "nal_from"
                  << std::setw(8) << "nal_to"
                  << "  frame_types\n";
        for (const GopInfo& gop : report.gops) {
            std::cout << std::left << std::setw(5) << gop.index
                      << std::right << std::setw(14) << gop.start_offset
                      << std::setw(14) << gop.byte_length
                      << std::setw(8) << gop.frame_count
                      << std::setw(10) << gop.first_nal
                      << std::setw(8) << gop.last_nal
                      << "  " << gop.frame_types << '\n';
        }
    }
    if (!report.warnings.empty()) {
        std::cout << "Warnings: count=" << report.warnings.size() << '\n';
        for (const Warning& warning : report.warnings) {
            std::cout << '[' << warning.level << "] offset=" << warning.offset
                      << " message=" << warning.message << '\n';
        }
    }
    if (show_summary) {
        std::cout << "Levels:";
        for (const auto& item : report.stats.warning_counts) {
            std::cout << ' ' << item.first << '=' << item.second;
        }
        std::cout << '\n';
    }
}

bool write_json(const std::string& path, const AuditReport& report, std::string& error) {
    if (path.empty()) {
        error = "JSON output path is empty";
        return false;
    }
    std::error_code ec;
    const std::filesystem::path output_path(path);
    const std::filesystem::path parent = output_path.parent_path();
    if (!parent.empty()) {
        std::filesystem::create_directories(parent, ec);
        if (ec) {
            error = "cannot create JSON output directory: " + ec.message();
            return false;
        }
    }
    std::ofstream out(path, std::ios::binary | std::ios::trunc);
    if (!out) {
        error = "cannot open JSON output file: " + path;
        return false;
    }
    out << "{\n";
    write_sps(out, report.sps);
    write_pps(out, report.pps);
    write_frames(out, report.stats);
    write_gops(out, report.gops);
    write_warnings(out, report.warnings);
    out << "}\n";
    out.close();
    if (!out) {
        error = "failed while writing JSON output: " + path;
        return false;
    }
    return true;
}

}  // namespace h264
