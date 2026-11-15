#include "access_unit.h"

#include "bit_reader.h"

#include <sstream>

namespace h264 {
namespace {

bool is_vcl(std::uint8_t type) {
    return type == 1 || type == 5;
}

std::string offset_message(const char* message, std::uint64_t offset) {
    std::ostringstream out;
    out << message << " at NAL offset " << offset;
    return out.str();
}

bool slice_matches_au(const AccessUnit& au, const SliceInfo& slice,
                      const SliceInfo& first, std::string& error) {
    if (slice.slice_type != first.slice_type) {
        error = offset_message("slice_type differs within an access unit", slice.nal_offset);
        return false;
    }
    if (slice.frame_num != first.frame_num) {
        error = offset_message("frame_num differs within an access unit", slice.nal_offset);
        return false;
    }
    if (slice.field_pic_flag != first.field_pic_flag ||
        slice.bottom_field_flag != first.bottom_field_flag) {
        error = offset_message("field flags differ within an access unit", slice.nal_offset);
        return false;
    }
    if (slice.pps_id != first.pps_id) {
        error = offset_message("PPS id differs within an access unit", slice.nal_offset);
        return false;
    }
    (void)au;
    return true;
}

}  // namespace

bool parse_recovery_point(const std::uint8_t* data, std::size_t size,
                          RecoveryPoint& out, std::string& error) {
    out = RecoveryPoint{};
    if (data == nullptr || size < 2) {
        error = "SEI NAL is truncated";
        return false;
    }
    BitReader reader(data + 1, size - 1);
    while (reader.bits_left() >= 16) {
        std::uint64_t payload_type = 0;
        std::uint64_t next = 0;
        do {
            next = reader.u(8);
            payload_type += next;
        } while (next == 255 && reader.ok());
        std::uint64_t payload_size = 0;
        do {
            next = reader.u(8);
            payload_size += next;
        } while (next == 255 && reader.ok());
        if (!reader.ok()) {
            error = "SEI message header: " + reader.error();
            return false;
        }
        if (payload_size > reader.bits_left() / 8) {
            error = "SEI payload exceeds NAL size";
            return false;
        }
        const std::size_t begin = reader.bit_position();
        const std::uint64_t end = begin + payload_size * 8;
        if (payload_type == 6) {
            out.present = true;
            out.recovery_frame_cnt = static_cast<std::uint32_t>(reader.ue());
            out.exact_match_flag = reader.u(1) != 0;
            out.broken_link_flag = reader.u(1) != 0;
            out.changing_slice_group_idc = static_cast<std::uint32_t>(reader.u(2));
            if (!reader.ok()) {
                error = "recovery_point SEI: " + reader.error();
                return false;
            }
        }
        if (reader.bit_position() > end) {
            error = "recovery_point SEI exceeds declared payload size";
            return false;
        }
        if (!reader.skip(static_cast<std::size_t>(end - reader.bit_position()))) {
            error = "SEI payload skip: " + reader.error();
            return false;
        }
    }
    return true;
}

bool build_access_units(const std::vector<NalUnit>& nals,
                        const std::vector<SliceInfo>& slices,
                        const std::map<std::size_t, RecoveryPoint>& recovery_points,
                        std::uint64_t file_size,
                        std::vector<AccessUnit>& aus,
                        std::string& error) {
    aus.clear();
    std::vector<std::size_t> slice_for_nal(nals.size(), static_cast<std::size_t>(-1));
    for (std::size_t i = 0; i < slices.size(); ++i) {
        if (slices[i].nal_index >= nals.size() || slice_for_nal[slices[i].nal_index] != static_cast<std::size_t>(-1)) {
            error = "slice-to-NAL mapping is inconsistent";
            return false;
        }
        slice_for_nal[slices[i].nal_index] = i;
    }

    AccessUnit current;
    bool active = false;
    bool has_vcl = false;
    auto start_au = [&](std::size_t nal_index) {
        current = AccessUnit{};
        current.first_nal = nal_index;
        current.last_nal = nal_index;
        current.start_offset = nals[nal_index].start_code_offset;
        active = true;
        has_vcl = false;
    };
    auto finish_au = [&](std::uint64_t end_offset) {
        current.end_offset = end_offset;
        current.has_aud = nals[current.first_nal].type == 9 ||
                          (current.first_nal + 1 < nals.size() && nals[current.first_nal + 1].type == 9);
        for (std::size_t i = current.first_nal; i <= current.last_nal; ++i) {
            const auto recovery = recovery_points.find(i);
            if (recovery != recovery_points.end() && recovery->second.present) {
                current.recovery_point = true;
                current.recovery = recovery->second;
            }
            if (nals[i].type == 9) {
                current.has_aud = true;
            }
        }
        if (has_vcl) {
            aus.push_back(current);
        }
        active = false;
    };

    for (std::size_t i = 0; i < nals.size(); ++i) {
        const NalUnit& nal = nals[i];
        if (nal.type == 9) {
            if (active) {
                if (!has_vcl) {
                    const std::size_t leading_first = current.first_nal;
                    const std::uint64_t leading_start = current.start_offset;
                    start_au(i);
                    current.first_nal = leading_first;
                    current.start_offset = leading_start;
                    current.has_aud = true;
                    continue;
                }
                finish_au(nal.start_code_offset);
            }
            start_au(i);
            current.has_aud = true;
            continue;
        }
        if (!is_vcl(nal.type)) {
            if (!active) {
                start_au(i);
            } else if (has_vcl && (nal.type == 6 || nal.type == 7 || nal.type == 8)) {
                finish_au(nal.start_code_offset);
                start_au(i);
            }
            current.last_nal = i;
            continue;
        }
        if (slice_for_nal[i] == static_cast<std::size_t>(-1)) {
            if (nal.at_eof) {
                current.last_nal = i;
                continue;
            }
            error = offset_message("VCL NAL has no parsed slice header", nal.offset);
            return false;
        }
        const SliceInfo& slice = slices[slice_for_nal[i]];
        if (!active) {
            start_au(i);
        }
        if (has_vcl && slice.first_mb_in_slice == 0) {
            finish_au(nal.start_code_offset);
            start_au(i);
        }
        if (has_vcl) {
            const SliceInfo& first = slices[current.first_slice];
            if (!slice_matches_au(current, slice, first, error)) {
                return false;
            }
        } else {
            if (slice.first_mb_in_slice != 0) {
                error = offset_message("first slice in an access unit has non-zero first_mb_in_slice", nal.offset);
                return false;
            }
            current.first_slice = slice_for_nal[i];
            current.slice_type = slice.slice_type;
            current.frame_num = slice.frame_num;
            current.field_pic_flag = slice.field_pic_flag;
            current.bottom_field_flag = slice.bottom_field_flag;
            current.idr = slice.idr;
        }
        ++current.slice_count;
        current.last_nal = i;
        has_vcl = true;
    }
    if (active) {
        finish_au(file_size);
    }
    if (aus.empty()) {
        error = "no access units containing coded slices";
        return false;
    }
    return true;
}

bool merge_field_pairs(const std::vector<AccessUnit>& aus,
                       std::vector<AccessUnit>& frames,
                       std::string& error) {
    frames.clear();
    for (const AccessUnit& au : aus) {
        if (!frames.empty()) {
            AccessUnit& previous = frames.back();
            const bool pair = previous.field_pic_flag && au.field_pic_flag &&
                              !previous.field_pair_merged &&
                              previous.frame_num == au.frame_num &&
                              previous.frame_num_unwrapped == au.frame_num_unwrapped &&
                              previous.bottom_field_flag != au.bottom_field_flag;
            if (pair) {
                if (previous.slice_type != au.slice_type) {
                    error = offset_message("field pair has inconsistent slice_type", au.start_offset);
                    return false;
                }
                if (previous.idr != au.idr) {
                    error = offset_message("field pair has inconsistent IDR status", au.start_offset);
                    return false;
                }
                previous.last_nal = au.last_nal;
                previous.end_offset = au.end_offset;
                previous.slice_count += au.slice_count;
                previous.field_pair_merged = true;
                previous.recovery_point = previous.recovery_point || au.recovery_point;
                if (au.recovery_point) {
                    previous.recovery = au.recovery;
                }
                if (au.poc < previous.poc) {
                    previous.poc = au.poc;
                }
                continue;
            }
            if (previous.field_pic_flag && au.field_pic_flag &&
                !previous.field_pair_merged && previous.frame_num == au.frame_num &&
                previous.bottom_field_flag == au.bottom_field_flag) {
                error = offset_message("two fields in a pair have the same parity", au.start_offset);
                return false;
            }
        }
        frames.push_back(au);
    }
    return true;
}

}  // namespace h264
