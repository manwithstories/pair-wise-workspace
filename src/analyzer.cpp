#include "analyzer.h"

#include "h264_headers.h"
#include "nal_scan.h"
#include "picture_assembler.h"

#include <cstdint>
#include <map>
#include <memory>
#include <string>
#include <utility>
#include <vector>

namespace h264scan {
namespace {

SliceMeta makeSliceMeta(const NalUnit& nal, const SliceHeader& header) {
    SliceMeta meta;
    meta.start_offset = nal.start_offset;
    meta.end_offset = nal.end_offset;
    meta.first_mb_in_slice = header.first_mb_in_slice;
    meta.slice_type = header.slice_type;
    meta.pps_id = header.pps_id;
    meta.frame_num = header.frame_num;
    meta.poc_lsb = header.pic_order_cnt_lsb;
    meta.idr_pic_id = header.idr_pic_id;
    meta.idr = header.idr;
    meta.field_pic = header.field_pic_flag;
    meta.bottom_field = header.bottom_field_flag;
    return meta;
}

}  // namespace

int analyzeFile(const std::string& path, bool strict, FileReport& report,
                std::string& error) {
    error.clear();
    GopReporter reporter(path);
    std::map<uint32_t, Sps> sps_by_id;
    std::map<uint32_t, Pps> pps_by_id;
    std::unique_ptr<PictureAssembler> assembler;
    uint32_t active_sps_id = 0;
    bool has_active_sps = false;
    std::string assembler_error;

    const auto warning = [&reporter](const std::string& message) {
        reporter.addWarning(message);
    };

    const auto activate_sps = [&](uint32_t sps_id) -> bool {
        const auto found = sps_by_id.find(sps_id);
        if (found == sps_by_id.end()) {
            return false;
        }
        if (assembler && has_active_sps && active_sps_id != sps_id) {
            if (!assembler->flush(assembler_error)) {
                return false;
            }
            assembler.reset();
        }
        if (!assembler) {
            assembler = std::make_unique<PictureAssembler>(
                found->second,
                [&reporter, &assembler_error](const Frame& frame) {
                    return reporter.addFrame(frame, assembler_error);
                },
                [&reporter](const std::string& message) {
                    reporter.addWarning(message);
                });
        }
        active_sps_id = sps_id;
        has_active_sps = true;
        if (!reporter.report().has_sps) {
            reporter.report().has_sps = true;
            reporter.report().sps = found->second;
        }
        return true;
    };

    const auto on_nal = [&](const NalUnit& nal) -> bool {
        const uint8_t type = nal.nal_unit_type;
        if (type == 7U) {
            Sps parsed;
            std::string parse_error;
            if (parseSpsNal(nal.data, parsed, parse_error)) {
                ++reporter.report().sps_count;
                sps_by_id[parsed.id] = parsed;
                if (!reporter.report().has_sps) {
                    reporter.report().has_sps = true;
                    reporter.report().sps = parsed;
                }
            } else {
                ++reporter.report().truncated_nal_count;
                warning("malformed SPS at offset " + std::to_string(nal.start_offset) +
                        ": " + parse_error);
            }
            return true;
        }

        if (type == 8U) {
            Pps parsed;
            std::string parse_error;
            if (parsePpsNal(nal.data, parsed, parse_error)) {
                ++reporter.report().pps_count;
                pps_by_id[parsed.id] = parsed;
                if (sps_by_id.find(parsed.sps_id) == sps_by_id.end()) {
                    warning("PPS " + std::to_string(parsed.id) +
                            " references unknown SPS " + std::to_string(parsed.sps_id));
                }
            } else {
                ++reporter.report().truncated_nal_count;
                warning("malformed PPS at offset " + std::to_string(nal.start_offset) +
                        ": " + parse_error);
            }
            return true;
        }

        if (type == 1U || type == 5U) {
            uint32_t pps_id = 0;
            std::string parse_error;
            if (!peekSlicePpsId(nal.data, pps_id, parse_error)) {
                ++reporter.report().truncated_nal_count;
                warning("truncated slice NAL at offset " +
                        std::to_string(nal.start_offset) + ": " + parse_error);
                return true;
            }
            const auto pps_it = pps_by_id.find(pps_id);
            if (pps_it == pps_by_id.end()) {
                warning("slice at offset " + std::to_string(nal.start_offset) +
                        " references unknown PPS " + std::to_string(pps_id));
                return true;
            }
            const auto sps_it = sps_by_id.find(pps_it->second.sps_id);
            if (sps_it == sps_by_id.end()) {
                warning("slice at offset " + std::to_string(nal.start_offset) +
                        " references unknown SPS " +
                        std::to_string(pps_it->second.sps_id));
                return true;
            }
            if ((!has_active_sps || active_sps_id != sps_it->first) &&
                !activate_sps(sps_it->first)) {
                error = assembler_error.empty() ? "failed to activate SPS" : assembler_error;
                return false;
            }

            SliceHeader header;
            if (!parseSliceHeaderNal(nal.data, sps_it->second, pps_it->second,
                                     header, parse_error)) {
                ++reporter.report().truncated_nal_count;
                warning("truncated slice NAL at offset " +
                        std::to_string(nal.start_offset) + ": " + parse_error);
                return true;
            }
            if (!assembler->addSlice(makeSliceMeta(nal, header), assembler_error)) {
                error = assembler_error.empty() ? "failed to assemble slices"
                                                : assembler_error;
                return false;
            }
            return true;
        }

        ++reporter.report().unknown_nal_count;
        return true;
    };

    ScanStats scan_stats;
    if (!scanAnnexBFile(path, on_nal, scan_stats, error)) {
        reporter.report().nal_count = scan_stats.nal_count;
        reporter.finish(error);
        report = std::move(reporter.report());
        return 2;
    }
    reporter.report().nal_count = scan_stats.nal_count;

    if (assembler && !assembler->flush(assembler_error)) {
        error = assembler_error.empty() ? "failed to flush picture assembler"
                                        : assembler_error;
        reporter.finish(error);
        report = std::move(reporter.report());
        return 2;
    }
    if (!reporter.finish(error)) {
        report = std::move(reporter.report());
        return 2;
    }

    report = std::move(reporter.report());
    if (!report.has_sps) {
        return 3;
    }
    if (strict && report.warning_count != 0U) {
        return 1;
    }
    return 0;
}

}  // namespace h264scan
