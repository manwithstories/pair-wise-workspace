#include "report.h"

#include <algorithm>
#include <fstream>
#include <ostream>
#include <sstream>

namespace h264 {

namespace {

struct FrameCounts {
    std::size_t i = 0;
    std::size_t p = 0;
    std::size_t b = 0;
};

FrameCounts countFramesIn(const FileAnalysis& analysis) {
    FrameCounts counts;
    for (const Sequence& sequence : analysis.sequences) {
        for (const Gop& gop : sequence.gops) {
            counts.i += gop.i_count;
            counts.p += gop.p_count;
            counts.b += gop.b_count;
        }
    }
    return counts;
}

std::size_t countAnomalies(const FileAnalysis& analysis) {
    return analysis.anomalies.size();
}

}  // namespace

void printProbe(std::ostream& out, const FileAnalysis& analysis) {
    const FrameCounts frames = countFramesIn(analysis);
    out << "file=" << analysis.path << " sequences=" << analysis.sequences.size()
        << " gops=" << totalGops(analysis) << " frames=" << totalFrames(analysis)
        << " i=" << frames.i << " p=" << frames.p << " b=" << frames.b
        << " slices=" << totalSlices(analysis)
        << " anomalous=" << countAnomalies(analysis) << '\n';
    out << "bytes=" << analysis.bytes_read << " nals=" << analysis.nal_count
        << " warnings=" << analysis.warnings.size() << '\n';

    for (const Sequence& sequence : analysis.sequences) {
        const Sps& sps = sequence.sps;
        out << "sequence=" << sequence.id << " start=" << sequence.start_offset
            << " end=" << sequence.end_offset << " sps_id=" << sps.id
            << " profile_idc=" << sps.profile_idc << " level_idc=" << sps.level_idc
            << '\n';
        out << "  sps chroma_format_idc=" << sps.chroma_format_idc
            << " bit_depth_luma=" << (8 + sps.bit_depth_luma_minus8)
            << " bit_depth_chroma=" << (8 + sps.bit_depth_chroma_minus8)
            << " log2_max_frame_num=" << sps.log2_max_frame_num
            << " max_num_ref_frames=" << sps.max_num_ref_frames
            << " poc_type=" << sps.pic_order_cnt_type
            << " log2_max_poc_lsb=" << sps.log2_max_pic_order_cnt_lsb
            << " delta_always_zero=" << (sps.delta_pic_order_always_zero_flag ? 1 : 0)
            << " offset_non_ref=" << sps.offset_for_non_ref_pic
            << " offset_top_bottom=" << sps.offset_for_top_to_bottom_field
            << " poc_cycle=" << sps.num_ref_frames_in_pic_order_cnt_cycle
            << " frame_mbs_only_flag=" << (sps.frame_mbs_only_flag ? 1 : 0)
            << " mbaff=" << (sps.mb_adaptive_frame_field_flag ? 1 : 0)
            << " width=" << sps.width() << " height=" << sps.height()
            << " crop=" << (sps.frame_cropping_flag ? 1 : 0)
            << " crop_l=" << sps.frame_crop_left_offset
            << " crop_r=" << sps.frame_crop_right_offset
            << " crop_t=" << sps.frame_crop_top_offset
            << " crop_b=" << sps.frame_crop_bottom_offset << '\n';
        if (sequence.has_pps) {
            const Pps& pps = sequence.pps;
            out << "  pps id=" << pps.id << " entropy="
                << (pps.entropy_coding_mode_flag ? "cabac" : "cavlc")
                << " num_slice_groups=" << pps.num_slice_groups()
                << " fmo_type=" << (pps.num_slice_groups() > 1 ? pps.slice_group_map_type : 0)
                << " weighted_pred=" << (pps.weighted_pred_flag ? 1 : 0)
                << " weighted_bipred_idc=" << pps.weighted_bipred_idc
                << " pic_init_qp=" << (26 + pps.pic_init_qp_minus26) << '\n';
            if (pps.num_slice_groups() > 1) {
                out << "  fmo map_type=" << pps.slice_group_map_type;
                if (pps.slice_group_map_type == 0) {
                    out << " run_length_minus1=";
                    for (std::size_t i = 0; i < pps.run_length_minus1.size(); ++i) {
                        if (i != 0) out << ',';
                        out << pps.run_length_minus1[i];
                    }
                } else if (pps.slice_group_map_type == 2) {
                    out << " top_left_bottom_right=";
                    for (std::size_t i = 0; i < pps.top_left_bottom_right.size(); ++i) {
                        if (i != 0) out << ',';
                        out << pps.top_left_bottom_right[i].first << ':'
                            << pps.top_left_bottom_right[i].second;
                    }
                } else if (pps.slice_group_map_type >= 3 &&
                           pps.slice_group_map_type <= 5) {
                    out << " direction=" << (pps.slice_group_change_direction_flag ? 1 : 0)
                        << " change_rate_minus1=" << pps.slice_group_change_rate_minus1;
                } else if (pps.slice_group_map_type == 6) {
                    out << " map_units=" << (pps.pic_size_in_map_units_minus1 + 1)
                        << " slice_group_ids=" << pps.slice_group_id.size();
                }
                out << '\n';
            }
        }
        for (const Gop& gop : sequence.gops) {
            out << "  gop sequence_id=" << gop.sequence_id
                << " start_offset=" << gop.start_offset
                << " end_offset=" << gop.end_offset
                << " frames=" << gop.frame_count << " i=" << gop.i_count
                << " p=" << gop.p_count << " b=" << gop.b_count
                << " slices=" << gop.slice_count
                << " interlaced=" << (gop.interlaced ? 1 : 0) << '\n';
        }
        for (const Gop& gop : sequence.gops) {
            for (const FrameRecord& frame : gop.frames) {
                const SliceInfo& first = frame.slices.front().info;
                out << "    frame offset=" << frame.offset
                    << " type=" << frameTypeName(frame.frame_type)
                    << " slice_type=" << first.slice_type
                    << " slice_type_raw=" << first.slice_type_raw
                    << " first_mb=" << first.first_mb_in_slice
                    << " frame_num=" << frame.frame_num
                    << " idr=" << (frame.idr ? 1 : 0)
                    << " idr_pic_id=" << first.idr_pic_id
                    << " field=" << (frame.field_pic_flag ? 1 : 0)
                    << " bottom=" << (frame.bottom_field_flag ? 1 : 0)
                    << " poc_lsb=" << frame.pic_order_cnt_lsb
                    << " slices=" << frame.sliceCount() << " first_mbs=";
                for (std::size_t i = 0; i < frame.slices.size(); ++i) {
                    if (i != 0) out << ',';
                    out << frame.slices[i].info.first_mb_in_slice;
                }
                out << '\n';
            }
        }
    }
    for (const Anomaly& anomaly : analysis.anomalies) {
        out << "anomaly offset=" << anomaly.offset << " kind=" << anomaly.kind
            << " message=" << anomaly.message << '\n';
    }
}

namespace {

std::string jsonEscape(const std::string& text) {
    std::ostringstream out;
    for (const unsigned char ch : text) {
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
                    const char* hex = "0123456789abcdef";
                    out << "\\u00" << hex[(ch >> 4) & 0xf] << hex[ch & 0xf];
                } else {
                    out << static_cast<char>(ch);
                }
        }
    }
    return out.str();
}

std::string quoted(const std::string& text) {
    return "\"" + jsonEscape(text) + "\"";
}

const char* boolText(bool value) {
    return value ? "true" : "false";
}

void writeSps(std::ostream& out, const Sps& sps) {
    out << "{\"id\":" << sps.id << ",\"profile_idc\":" << sps.profile_idc
        << ",\"level_idc\":" << sps.level_idc
        << ",\"chroma_format_idc\":" << sps.chroma_format_idc
        << ",\"separate_colour_plane_flag\":" << boolText(sps.separate_colour_plane_flag)
        << ",\"bit_depth_luma_minus8\":" << sps.bit_depth_luma_minus8
        << ",\"bit_depth_chroma_minus8\":" << sps.bit_depth_chroma_minus8
        << ",\"bit_depth_luma\":" << (8 + sps.bit_depth_luma_minus8)
        << ",\"bit_depth_chroma\":" << (8 + sps.bit_depth_chroma_minus8)
        << ",\"log2_max_frame_num\":" << sps.log2_max_frame_num
        << ",\"pic_order_cnt_type\":" << sps.pic_order_cnt_type
        << ",\"log2_max_pic_order_cnt_lsb\":" << sps.log2_max_pic_order_cnt_lsb
        << ",\"delta_pic_order_always_zero_flag\":"
        << boolText(sps.delta_pic_order_always_zero_flag)
        << ",\"offset_for_non_ref_pic\":" << sps.offset_for_non_ref_pic
        << ",\"offset_for_top_to_bottom_field\":" << sps.offset_for_top_to_bottom_field
        << ",\"num_ref_frames_in_pic_order_cnt_cycle\":"
        << sps.num_ref_frames_in_pic_order_cnt_cycle << ",\"offset_for_ref_frame\":[";
    for (std::size_t i = 0; i < sps.offset_for_ref_frame.size(); ++i) {
        if (i != 0) out << ',';
        out << sps.offset_for_ref_frame[i];
    }
    out << "],\"max_num_ref_frames\":" << sps.max_num_ref_frames
        << ",\"gaps_in_frame_num_value_allowed_flag\":"
        << boolText(sps.gaps_in_frame_num_value_allowed_flag)
        << ",\"pic_width_in_mbs_minus1\":" << sps.pic_width_in_mbs_minus1
        << ",\"pic_height_in_map_units_minus1\":" << sps.pic_height_in_map_units_minus1
        << ",\"width\":" << sps.width() << ",\"height\":" << sps.height()
        << ",\"frame_mbs_only_flag\":" << boolText(sps.frame_mbs_only_flag)
        << ",\"mb_adaptive_frame_field_flag\":"
        << boolText(sps.mb_adaptive_frame_field_flag)
        << ",\"direct_8x8_inference_flag\":" << boolText(sps.direct_8x8_inference_flag)
        << ",\"frame_cropping_flag\":" << boolText(sps.frame_cropping_flag)
        << ",\"frame_crop_left_offset\":" << sps.frame_crop_left_offset
        << ",\"frame_crop_right_offset\":" << sps.frame_crop_right_offset
        << ",\"frame_crop_top_offset\":" << sps.frame_crop_top_offset
        << ",\"frame_crop_bottom_offset\":" << sps.frame_crop_bottom_offset << '}';
}

void writePps(std::ostream& out, const Pps& pps) {
    out << "{\"id\":" << pps.id << ",\"sps_id\":" << pps.spsId
        << ",\"entropy_coding_mode_flag\":" << boolText(pps.entropy_coding_mode_flag)
        << ",\"bottom_field_pic_order_in_frame_present_flag\":"
        << boolText(pps.bottom_field_pic_order_in_frame_present_flag)
        << ",\"num_slice_groups\":" << pps.num_slice_groups()
        << ",\"slice_group_map_type\":" << pps.slice_group_map_type
        << ",\"run_length_minus1\":[";
    for (std::size_t i = 0; i < pps.run_length_minus1.size(); ++i) {
        if (i != 0) out << ',';
        out << pps.run_length_minus1[i];
    }
    out << "],\"top_left_bottom_right\":[";
    for (std::size_t i = 0; i < pps.top_left_bottom_right.size(); ++i) {
        if (i != 0) out << ',';
        out << "{\"top_left\":" << pps.top_left_bottom_right[i].first
            << ",\"bottom_right\":" << pps.top_left_bottom_right[i].second << '}';
    }
    out << "],\"slice_group_change_direction_flag\":"
        << boolText(pps.slice_group_change_direction_flag)
        << ",\"slice_group_change_rate_minus1\":" << pps.slice_group_change_rate_minus1
        << ",\"pic_size_in_map_units_minus1\":" << pps.pic_size_in_map_units_minus1
        << ",\"slice_group_id\":[";
    for (std::size_t i = 0; i < pps.slice_group_id.size(); ++i) {
        if (i != 0) out << ',';
        out << pps.slice_group_id[i];
    }
    out << "],\"num_ref_idx_l0_default_active_minus1\":"
        << pps.num_ref_idx_l0_default_active_minus1
        << ",\"num_ref_idx_l1_default_active_minus1\":"
        << pps.num_ref_idx_l1_default_active_minus1
        << ",\"weighted_pred_flag\":" << boolText(pps.weighted_pred_flag)
        << ",\"weighted_bipred_idc\":" << pps.weighted_bipred_idc
        << ",\"pic_init_qp_minus26\":" << pps.pic_init_qp_minus26
        << ",\"pic_init_qp\":" << (26 + pps.pic_init_qp_minus26)
        << ",\"pic_init_qs_minus26\":" << pps.pic_init_qs_minus26
        << ",\"chroma_qp_index_offset\":" << pps.chroma_qp_index_offset
        << ",\"deblocking_filter_control_present_flag\":"
        << boolText(pps.deblocking_filter_control_present_flag)
        << ",\"constrained_intra_pred_flag\":"
        << boolText(pps.constrained_intra_pred_flag)
        << ",\"redundant_pic_cnt_present_flag\":"
        << boolText(pps.redundant_pic_cnt_present_flag) << '}';
}

void writeSlice(std::ostream& out, const SliceRecord& record) {
    const SliceInfo& info = record.info;
    out << "{\"offset\":" << record.offset
        << ",\"first_mb_in_slice\":" << info.first_mb_in_slice
        << ",\"slice_type_raw\":" << info.slice_type_raw
        << ",\"slice_type\":" << info.slice_type
        << ",\"frame_type\":" << quoted(frameTypeName(info.frame_type))
        << ",\"pps_id\":" << info.pps_id
        << ",\"frame_num\":" << info.frame_num
        << ",\"idr\":" << boolText(info.idr)
        << ",\"field_pic_flag\":" << boolText(info.field_pic_flag)
        << ",\"bottom_field_flag\":" << boolText(info.bottom_field_flag)
        << ",\"has_colour_plane_id\":" << boolText(info.has_colour_plane_id)
        << ",\"colour_plane_id\":" << info.colour_plane_id
        << ",\"idr_pic_id\":" << info.idr_pic_id
        << ",\"pic_order_cnt_lsb\":" << info.pic_order_cnt_lsb
        << ",\"has_delta_poc_bottom\":" << boolText(info.has_delta_poc_bottom)
        << ",\"delta_pic_order_cnt_bottom\":" << info.delta_pic_order_cnt_bottom
        << ",\"has_delta_poc\":" << boolText(info.has_delta_poc)
        << ",\"delta_pic_order_cnt0\":" << info.delta_pic_order_cnt0
        << ",\"delta_pic_order_cnt1\":" << info.delta_pic_order_cnt1
        << ",\"redundant_pic_cnt\":" << info.redundant_pic_cnt << '}';
}

void writeFrame(std::ostream& out, const FrameRecord& frame) {
    out << "{\"offset\":" << frame.offset << ",\"end_offset\":" << frame.end_offset
        << ",\"frame_num\":" << frame.frame_num
        << ",\"frame_type\":" << quoted(frameTypeName(frame.frame_type))
        << ",\"idr\":" << boolText(frame.idr)
        << ",\"field_pic_flag\":" << boolText(frame.field_pic_flag)
        << ",\"bottom_field_flag\":" << boolText(frame.bottom_field_flag)
        << ",\"interlaced\":" << boolText(frame.interlaced)
        << ",\"pic_order_cnt_lsb\":" << frame.pic_order_cnt_lsb
        << ",\"slice_count\":" << frame.sliceCount() << ",\"slices\":[";
    for (std::size_t i = 0; i < frame.slices.size(); ++i) {
        if (i != 0) out << ',';
        writeSlice(out, frame.slices[i]);
    }
    out << "]}";
}

void writeGop(std::ostream& out, const Gop& gop) {
    out << "{\"sequence_id\":" << gop.sequence_id
        << ",\"start_offset\":" << gop.start_offset
        << ",\"end_offset\":" << gop.end_offset
        << ",\"frame_count\":" << gop.frame_count
        << ",\"i_count\":" << gop.i_count << ",\"p_count\":" << gop.p_count
        << ",\"b_count\":" << gop.b_count
        << ",\"slice_count\":" << gop.slice_count
        << ",\"interlaced\":" << boolText(gop.interlaced) << ",\"frames\":[";
    for (std::size_t i = 0; i < gop.frames.size(); ++i) {
        if (i != 0) out << ',';
        writeFrame(out, gop.frames[i]);
    }
    out << "]}";
}

struct SequenceCounts {
    std::size_t frames = 0;
    std::size_t i = 0;
    std::size_t p = 0;
    std::size_t b = 0;
    std::size_t slices = 0;
    bool interlaced = false;
};

SequenceCounts countSequence(const Sequence& sequence) {
    SequenceCounts counts;
    for (const Gop& gop : sequence.gops) {
        counts.frames += gop.frame_count;
        counts.i += gop.i_count;
        counts.p += gop.p_count;
        counts.b += gop.b_count;
        counts.slices += gop.slice_count;
        counts.interlaced = counts.interlaced || gop.interlaced;
    }
    counts.interlaced = counts.interlaced || !sequence.sps.frame_mbs_only_flag;
    return counts;
}

void writeSequence(std::ostream& out, const Sequence& sequence, const std::string& file) {
    const SequenceCounts counts = countSequence(sequence);
    out << "{\"file\":" << quoted(file) << ",\"id\":" << sequence.id
        << ",\"start_offset\":" << sequence.start_offset
        << ",\"end_offset\":" << sequence.end_offset
        << ",\"sps_offset\":" << sequence.sps_offset
        << ",\"frame_count\":" << counts.frames << ",\"i_count\":" << counts.i
        << ",\"p_count\":" << counts.p << ",\"b_count\":" << counts.b
        << ",\"slice_count\":" << counts.slices
        << ",\"interlaced\":" << boolText(counts.interlaced)
        << ",\"sps\":";
    writeSps(out, sequence.sps);
    out << ",\"pps\":";
    if (sequence.has_pps) {
        writePps(out, sequence.pps);
    } else {
        out << "null";
    }
    out << ",\"gops\":[";
    for (std::size_t i = 0; i < sequence.gops.size(); ++i) {
        if (i != 0) out << ',';
        writeGop(out, sequence.gops[i]);
    }
    out << "]}";
}

void writeAnomaly(std::ostream& out, const Anomaly& anomaly, const std::string& file) {
    out << "{\"file\":" << quoted(file) << ",\"offset\":" << anomaly.offset
        << ",\"kind\":" << quoted(anomaly.kind)
        << ",\"message\":" << quoted(anomaly.message) << '}';
}

void writeIndexJsonDocument(std::ostream& out, const std::vector<FileAnalysis>& analyses) {
    std::size_t sequenceCount = 0;
    std::size_t gopCount = 0;
    std::size_t frameCount = 0;
    std::size_t sliceCount = 0;
    std::size_t anomalyCount = 0;
    for (const FileAnalysis& analysis : analyses) {
        sequenceCount += analysis.sequences.size();
        gopCount += totalGops(analysis);
        frameCount += totalFrames(analysis);
        sliceCount += totalSlices(analysis);
        anomalyCount += analysis.anomalies.size();
    }

    out << "{\"format\":\"h264gop-index-v1\",\"file_count\":" << analyses.size()
        << ",\"sequence_count\":" << sequenceCount << ",\"gop_count\":" << gopCount
        << ",\"frame_count\":" << frameCount << ",\"slice_count\":" << sliceCount
        << ",\"anomalous\":" << anomalyCount << ",\"files\":[";
    for (std::size_t i = 0; i < analyses.size(); ++i) {
        if (i != 0) out << ',';
        const FileAnalysis& analysis = analyses[i];
        out << "{\"path\":" << quoted(analysis.path)
            << ",\"bytes_read\":" << analysis.bytes_read
            << ",\"nal_count\":" << analysis.nal_count
            << ",\"sequence_count\":" << analysis.sequences.size()
            << ",\"gop_count\":" << totalGops(analysis)
            << ",\"frame_count\":" << totalFrames(analysis)
            << ",\"slice_count\":" << totalSlices(analysis)
            << ",\"anomalous\":" << analysis.anomalies.size()
            << ",\"warning_count\":" << analysis.warnings.size() << '}';
    }
    out << "],\"sequences\":[";
    bool first = true;
    for (const FileAnalysis& analysis : analyses) {
        for (const Sequence& sequence : analysis.sequences) {
            if (!first) out << ',';
            first = false;
            writeSequence(out, sequence, analysis.path);
        }
    }
    out << "],\"anomalies\":[";
    first = true;
    for (const FileAnalysis& analysis : analyses) {
        for (const Anomaly& anomaly : analysis.anomalies) {
            if (!first) out << ',';
            first = false;
            writeAnomaly(out, anomaly, analysis.path);
        }
    }
    out << "]}";
}

}  // namespace

void printIndexSummary(std::ostream& out, const std::vector<FileAnalysis>& analyses,
                       const std::string& jsonPath) {
    std::size_t sequences = 0;
    std::size_t gops = 0;
    std::size_t frames = 0;
    std::size_t anomalies = 0;
    for (const FileAnalysis& analysis : analyses) {
        sequences += analysis.sequences.size();
        gops += totalGops(analysis);
        frames += totalFrames(analysis);
        anomalies += analysis.anomalies.size();
    }
    out << "indexed files=" << analyses.size() << " sequences=" << sequences
        << " gops=" << gops << " frames=" << frames << " anomalous=" << anomalies;
    if (!jsonPath.empty()) {
        out << " json=" << jsonPath;
    }
    out << '\n';
}

bool writeIndexJson(const std::string& path, const std::vector<FileAnalysis>& analyses,
                    std::string& error) {
    std::ofstream output(path, std::ios::binary | std::ios::trunc);
    if (!output) {
        error = "cannot open JSON output: " + path;
        return false;
    }
    writeIndexJsonDocument(output, analyses);
    output << '\n';
    if (!output) {
        error = "failed while writing JSON output: " + path;
        return false;
    }
    return true;
}

}  // namespace h264
