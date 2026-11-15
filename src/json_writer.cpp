#include "json_writer.h"

#include <filesystem>
#include <fstream>
#include <ostream>
#include <string>
#include <vector>

namespace h264scan {
namespace {

void writeIndent(std::ostream& output, unsigned int spaces) {
    for (unsigned int i = 0; i < spaces; ++i) {
        output.put(' ');
    }
}

void writeJsonString(std::ostream& output, const std::string& value) {
    output.put('"');
    for (unsigned char ch : value) {
        switch (ch) {
            case '"': output << "\\\""; break;
            case '\\': output << "\\\\"; break;
            case '\b': output << "\\b"; break;
            case '\f': output << "\\f"; break;
            case '\n': output << "\\n"; break;
            case '\r': output << "\\r"; break;
            case '\t': output << "\\t"; break;
            default:
                if (ch < 0x20U) {
                    const char hex[] = "0123456789abcdef";
                    output << "\\u00" << hex[(ch >> 4U) & 0x0fU]
                           << hex[ch & 0x0fU];
                } else {
                    output.put(static_cast<char>(ch));
                }
        }
    }
    output.put('"');
}

bool ensureParentDirectory(const std::string& path, std::string& error) {
    const std::filesystem::path parent = std::filesystem::path(path).parent_path();
    if (parent.empty()) {
        return true;
    }
    std::error_code filesystem_error;
    std::filesystem::create_directories(parent, filesystem_error);
    if (filesystem_error) {
        error = "cannot create output directory: " + filesystem_error.message();
        return false;
    }
    return true;
}

void writeSps(std::ostream& output, const Sps& sps, unsigned int indent) {
    writeIndent(output, indent);
    output << "{\n";
    writeIndent(output, indent + 2U);
    output << "\"id\": " << sps.id << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"profile_idc\": " << sps.profile_idc << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"level_idc\": " << sps.level_idc << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"chroma_format_idc\": " << sps.chroma_format_idc << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"width\": " << sps.width << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"height\": " << sps.height << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"coded_width\": " << sps.coded_width << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"coded_height\": " << sps.coded_height << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"frame_mbs_only_flag\": "
           << (sps.frame_mbs_only_flag ? "true" : "false") << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"log2_max_frame_num_minus4\": " << sps.log2_max_frame_num_minus4
           << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"pic_order_cnt_type\": " << sps.pic_order_cnt_type << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"bit_depth_luma_minus8\": " << sps.bit_depth_luma_minus8 << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"bit_depth_chroma_minus8\": " << sps.bit_depth_chroma_minus8 << "\n";
    writeIndent(output, indent);
    output << "}";
}

void writeGop(std::ostream& output, const GopStats& gop, unsigned int indent) {
    writeIndent(output, indent);
    output << "{\n";
    const auto field = [&](const char* name, uint64_t value, bool comma) {
        writeIndent(output, indent + 2U);
        output << '"' << name << "\": " << value << (comma ? ",\n" : "\n");
    };
    field("index", gop.index, true);
    field("start_offset", gop.start_offset, true);
    field("byte_length", gop.byte_length, true);
    field("frames", gop.frames, true);
    field("i", gop.i_frames, true);
    field("p", gop.p_frames, true);
    field("b", gop.b_frames, true);
    field("field_pairs", gop.field_pairs, true);
    field("slice_count", gop.slice_count, true);
    field("missing_slices", gop.missing_slices, true);
    field("incomplete_frames", gop.incomplete_frames, true);
    field("warnings", gop.warnings, false);
    writeIndent(output, indent);
    output << "}";
}

void writeReport(std::ostream& output, const FileReport& report,
                 unsigned int indent) {
    writeIndent(output, indent);
    output << "{\n";
    writeIndent(output, indent + 2U);
    output << "\"file\": ";
    writeJsonString(output, report.file);
    output << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"sps\": ";
    if (report.has_sps) {
        output << '\n';
        writeSps(output, report.sps, indent + 2U);
    } else {
        output << "null";
    }
    output << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"nal_count\": " << report.nal_count << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"sps_count\": " << report.sps_count << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"pps_count\": " << report.pps_count << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"unknown_nal_count\": " << report.unknown_nal_count << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"truncated_nal_count\": " << report.truncated_nal_count << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"gops\": [";
    if (!report.gops.empty()) {
        output << '\n';
        for (size_t i = 0; i < report.gops.size(); ++i) {
            writeGop(output, report.gops[i], indent + 4U);
            output << (i + 1U < report.gops.size() ? ",\n" : "\n");
        }
        writeIndent(output, indent + 2U);
    }
    output << "],\n";

    writeIndent(output, indent + 2U);
    output << "\"totals\": {\n";
    const auto total = [&](const char* name, uint64_t value, bool comma) {
        writeIndent(output, indent + 4U);
        output << '"' << name << "\": " << value << (comma ? ",\n" : "\n");
    };
    total("frames", report.totals.frames, true);
    total("gop_count", report.totals.gop_count, true);
    total("slice_count", report.totals.slice_count, true);
    total("i", report.totals.i_frames, true);
    total("p", report.totals.p_frames, true);
    total("b", report.totals.b_frames, true);
    total("field_pairs", report.totals.field_pairs, true);
    total("missing_slices", report.totals.missing_slices, true);
    total("incomplete_frames", report.totals.incomplete_frames, true);
    total("warnings", report.totals.warnings, false);
    writeIndent(output, indent + 2U);
    output << "},\n";

    writeIndent(output, indent + 2U);
    output << "\"warning_count\": " << report.warning_count << ",\n";
    writeIndent(output, indent + 2U);
    output << "\"warning_details\": [";
    if (!report.warnings.empty()) {
        output << '\n';
        for (size_t i = 0; i < report.warnings.size(); ++i) {
            writeIndent(output, indent + 4U);
            writeJsonString(output, report.warnings[i]);
            output << (i + 1U < report.warnings.size() ? ",\n" : "\n");
        }
        writeIndent(output, indent + 2U);
    }
    output << "]\n";
    writeIndent(output, indent);
    output << "}";
}

}  // namespace

bool writeJsonReports(const std::vector<FileReport>& reports, const std::string& path,
                      std::string& error) {
    if (!ensureParentDirectory(path, error)) {
        return false;
    }
    std::ofstream output(path, std::ios::trunc);
    if (!output) {
        error = "cannot open JSON output: " + path;
        return false;
    }

    if (reports.size() == 1U) {
        writeReport(output, reports.front(), 0U);
        output << '\n';
    } else {
        output << "{\n  \"files\": [\n";
        for (size_t i = 0; i < reports.size(); ++i) {
            writeReport(output, reports[i], 4U);
            output << (i + 1U < reports.size() ? ",\n" : "\n");
        }
        output << "  ]\n}\n";
    }
    output.flush();
    if (!output) {
        error = "failed to write JSON output";
        return false;
    }
    return true;
}

namespace {

std::string csvCell(const std::string& value) {
    if (value.find_first_of(",\"\r\n") == std::string::npos) {
        return value;
    }
    std::string escaped = "\"";
    for (char ch : value) {
        if (ch == '"') {
            escaped += "\"\"";
        } else {
            escaped.push_back(ch);
        }
    }
    escaped.push_back('"');
    return escaped;
}

}  // namespace

bool writeCsvReports(const std::vector<FileReport>& reports, const std::string& path,
                     std::string& error) {
    if (!ensureParentDirectory(path, error)) {
        return false;
    }
    std::ofstream output(path, std::ios::trunc);
    if (!output) {
        error = "cannot open CSV output: " + path;
        return false;
    }

    output << "file,nal_count,gop_count,frames,slice_count,i,p,b,field_pairs,"
              "missing_slices,incomplete_frames,warnings\n";
    for (const FileReport& report : reports) {
        output << csvCell(report.file) << ',' << report.nal_count << ','
               << report.totals.gop_count << ',' << report.totals.frames << ','
               << report.totals.slice_count << ',' << report.totals.i_frames << ','
               << report.totals.p_frames << ',' << report.totals.b_frames << ','
               << report.totals.field_pairs << ',' << report.totals.missing_slices
               << ',' << report.totals.incomplete_frames << ','
               << report.totals.warnings << '\n';
    }
    output.flush();
    if (!output) {
        error = "failed to write CSV output";
        return false;
    }
    return true;
}

}  // namespace h264scan
