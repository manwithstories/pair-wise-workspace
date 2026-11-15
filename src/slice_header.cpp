#include "slice_header.h"

#include "bit_reader.h"

#include <sstream>

namespace h264 {
namespace {

bool reader_failure(const BitReader& reader, const char* prefix, std::string& error) {
    if (reader.ok()) {
        return false;
    }
    error = std::string(prefix) + ": " + reader.error();
    return true;
}

bool parse_ref_pic_marking(BitReader& reader, SliceInfo& slice, bool idr, std::string& error) {
    if (idr) {
        slice.no_output_of_prior_pics_flag = reader.u(1) != 0;
        slice.long_term_reference_flag = reader.u(1) != 0;
        return true;
    }
    slice.adaptive_ref_pic_marking_mode_flag = reader.u(1) != 0;
    if (!slice.adaptive_ref_pic_marking_mode_flag) {
        return true;
    }
    for (int count = 0; count < 1000; ++count) {
        const std::uint64_t operation = reader.ue();
        if (!reader.ok()) {
            error = "dec_ref_pic_marking memory_management_control_operation: " + reader.error();
            return false;
        }
        if (operation == 0) {
            return true;
        }
        if (operation > 6) {
            error = "memory_management_control_operation must be in [0,6]";
            return false;
        }
        if (operation == 1 || operation == 3) {
            reader.ue();
        }
        if (operation == 2) {
            reader.ue();
        }
        if (operation == 3 || operation == 6) {
            reader.ue();
        }
        if (operation == 4) {
            reader.ue();
        }
        if (!reader.ok()) {
            error = "dec_ref_pic_marking operands: " + reader.error();
            return false;
        }
    }
    error = "dec_ref_pic_marking exceeds 1000 operations";
    return false;
}

bool parse_ref_pic_list_modification(BitReader& reader, int slice_type, std::string& error) {
    if (slice_type != 2 && slice_type != 4) {
        if (reader.u(1) != 0) {
            for (int count = 0; count < 1000; ++count) {
                const std::uint64_t idc = reader.ue();
                if (idc == 3) break;
                if (idc > 2) {
                    error = "ref_pic_list_modification idc must be in [0,3]";
                    return false;
                }
                reader.ue();
                if (idc == 2) reader.ue();
                if (!reader.ok()) {
                    error = "ref_pic_list_modification l0: " + reader.error();
                    return false;
                }
            }
        }
    }
    if (slice_type == 1 && reader.u(1) != 0) {
        for (int count = 0; count < 1000; ++count) {
            const std::uint64_t idc = reader.ue();
            if (idc == 3) break;
            if (idc > 2) {
                error = "ref_pic_list_modification idc must be in [0,3]";
                return false;
            }
            reader.ue();
            if (idc == 2) reader.ue();
            if (!reader.ok()) {
                error = "ref_pic_list_modification l1: " + reader.error();
                return false;
            }
        }
    }
    return true;
}

bool parse_pred_weight_table(BitReader& reader, int slice_type, std::uint32_t chroma_format_idc,
                             int refs_l0, int refs_l1, std::string& error) {
    reader.ue();
    if (chroma_format_idc != 0) reader.ue();
    for (int list = 0; list < 2; ++list) {
        const int refs = list == 0 ? refs_l0 : refs_l1;
        if (list == 1 && slice_type != 1) continue;
        for (int i = 0; i < refs; ++i) {
            if (reader.u(1) != 0) { reader.se(); reader.se(); }
            if (chroma_format_idc != 0 && reader.u(1) != 0) {
                reader.se(); reader.se(); reader.se(); reader.se();
            }
            if (!reader.ok()) {
                error = "pred_weight_table: " + reader.error();
                return false;
            }
        }
    }
    return true;
}

}  // namespace

bool parse_slice_header(const NalUnit& nal, const std::uint8_t* data, std::size_t size,
                        const ParamSets& params, SliceInfo& out, std::string& error) {
    if (data == nullptr || size < 2) {
        error = "slice NAL is truncated";
        return false;
    }
    if (nal.type != 1 && nal.type != 5) {
        error = "NAL is not a coded slice";
        return false;
    }
    BitReader reader(data + 1, size - 1);
    out = SliceInfo{};
    out.nal_index = nal.index;
    out.nal_offset = nal.offset;
    out.start_code_offset = nal.start_code_offset;
    out.nal_ref_idc = nal.nal_ref_idc;
    out.idr = nal.type == 5;
    out.first_mb_in_slice = static_cast<std::uint32_t>(reader.ue());
    out.slice_type_raw = static_cast<int>(reader.ue());
    if (out.slice_type_raw > 9) {
        error = "slice_type must be in [0,9]";
        return false;
    }
    out.slice_type = out.slice_type_raw % 5;
    out.pps_id = static_cast<std::uint32_t>(reader.ue());
    if (!reader.ok()) {
        error = "slice header prefix: " + reader.error();
        return false;
    }
    const auto pps_it = params.pps.find(out.pps_id);
    if (pps_it == params.pps.end()) {
        error = "pic_parameter_set_id references unknown PPS " + std::to_string(out.pps_id);
        return false;
    }
    const PpsInfo& pps = pps_it->second;
    out.sps_id = pps.sps_id;
    const auto sps_it = params.sps.find(pps.sps_id);
    if (sps_it == params.sps.end()) {
        error = "PPS references unknown SPS " + std::to_string(pps.sps_id);
        return false;
    }
    const SpsInfo& sps = sps_it->second;
    if (sps.separate_colour_plane_flag) {
        reader.u(2);
    }
    out.frame_num = static_cast<std::uint32_t>(reader.u(static_cast<int>(sps.log2_max_frame_num)));
    if (!sps.frame_mbs_only_flag) {
        out.field_pic_flag = reader.u(1) != 0;
        if (out.field_pic_flag) {
            out.bottom_field_flag = reader.u(1) != 0;
        }
    }
    if (out.idr) {
        out.idr_pic_id = static_cast<std::uint32_t>(reader.ue());
    }
    if (sps.pic_order_cnt_type == 0) {
        out.pic_order_cnt_lsb = static_cast<std::uint32_t>(
            reader.u(static_cast<int>(sps.log2_max_pic_order_cnt_lsb)));
        if (pps.bottom_field_pic_order_in_frame_present_flag && !out.field_pic_flag) {
            out.delta_pic_order_cnt_bottom = reader.se();
        }
    } else if (sps.pic_order_cnt_type == 1 && !sps.delta_pic_order_always_zero_flag) {
        out.delta_pic_order_cnt[0] = reader.se();
        if (pps.bottom_field_pic_order_in_frame_present_flag && !out.field_pic_flag) {
            out.delta_pic_order_cnt[1] = reader.se();
        }
    }
    if (pps.redundant_pic_cnt_present_flag) {
        reader.ue();
    }
    if (out.slice_type == 1) {
        reader.u(1);
    }
    std::uint64_t refs_l0 = pps.num_ref_idx_l0_default_active_minus1 + 1U;
    std::uint64_t refs_l1 = pps.num_ref_idx_l1_default_active_minus1 + 1U;
    if (out.slice_type != 2 && out.slice_type != 4) {
        if (reader.u(1) != 0) {
            refs_l0 = reader.ue() + 1U;
            if (out.slice_type == 1) {
                refs_l1 = reader.ue() + 1U;
            }
        }
        if (refs_l0 > 32 || refs_l1 > 32) {
            error = "active reference index count exceeds 32";
            return false;
        }
        if (!parse_ref_pic_list_modification(reader, out.slice_type, error)) {
            return false;
        }
        const bool weighted = (pps.weighted_pred_flag && (out.slice_type == 0 || out.slice_type == 3)) ||
                              (pps.weighted_bipred_idc == 1 && out.slice_type == 1);
        if (weighted && !parse_pred_weight_table(reader, out.slice_type, sps.chroma_format_idc,
                                                 static_cast<int>(refs_l0), static_cast<int>(refs_l1), error)) {
            return false;
        }
    }
    if (reader_failure(reader, "slice header", error)) {
        return false;
    }
    if (out.nal_ref_idc != 0 &&
        !parse_ref_pic_marking(reader, out, out.idr, error)) {
        return false;
    }
    std::uint64_t picture_mbs = static_cast<std::uint64_t>(sps.pic_width_in_mbs) *
                                sps.pic_height_in_map_units;
    if (!sps.frame_mbs_only_flag && !out.field_pic_flag) {
        picture_mbs *= 2U;
    }
    if (out.first_mb_in_slice >= picture_mbs) {
        error = "first_mb_in_slice is outside the picture";
        return false;
    }
    return true;
}

}  // namespace h264
