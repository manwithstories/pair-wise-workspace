#include "picture_assembler.h"

#include <algorithm>
#include <array>
#include <cstddef>
#include <set>
#include <utility>

namespace h264scan {
namespace {

constexpr size_t kActiveFrameLimit = 8U;

auto makeKey(const SliceMeta& slice) {
    const uint32_t poc_key = slice.field_pic ? slice.poc_lsb / 2U : slice.poc_lsb;
    return std::tuple<uint32_t, uint32_t, bool, bool, uint32_t, uint32_t>{
        slice.pps_id, slice.frame_num, slice.field_pic, slice.idr,
        slice.idr_pic_id, poc_key};
}

uint32_t normalizeType(uint32_t slice_type) {
    const uint32_t type = slice_type % 5U;
    if (type == 1U) return 1U;
    if (type == 2U || type == 4U) return 2U;
    return 0U;
}

}  // namespace

PictureAssembler::PictureAssembler(const Sps& sps, FrameCallback frame_callback,
                                   WarningCallback warning_callback)
    : sps_(sps),
      frame_callback_(std::move(frame_callback)),
      warning_callback_(std::move(warning_callback)) {}

bool PictureAssembler::addSlice(const SliceMeta& slice, std::string& error) {
    if (slice.idr) {
        const IdrSignature signature{slice.frame_num, slice.poc_lsb, slice.idr_pic_id};
        if (have_idr_signature_ && signature != last_idr_signature_ &&
            !finalizeAll(error)) {
            return false;
        }
        last_idr_signature_ = signature;
        have_idr_signature_ = true;
    }

    const FrameKey key = makeKey(slice);
    auto found = active_.find(key);
    if (found == active_.end()) {
        FrameBuilder builder;
        builder.idr = slice.idr;
        builder.pps_id = slice.pps_id;
        builder.frame_num = slice.frame_num;
        builder.poc_lsb = slice.poc_lsb;
        builder.field_pic = slice.field_pic;
        found = active_.emplace(key, std::move(builder)).first;
        order_.push_back(key);
    }
    found->second.slices.push_back(slice);
    if (slice.field_pic) {
        found->second.poc_lsb = std::min(found->second.poc_lsb, slice.poc_lsb);
    }

    while (active_.size() > kActiveFrameLimit) {
        if (!finalizeOldest(error)) {
            return false;
        }
    }
    return true;
}

bool PictureAssembler::flush(std::string& error) {
    return finalizeAll(error);
}

bool PictureAssembler::finalizeOldest(std::string& error) {
    while (!order_.empty()) {
        const FrameKey key = order_.front();
        order_.pop_front();
        auto found = active_.find(key);
        if (found == active_.end()) {
            continue;
        }
        FrameBuilder builder = std::move(found->second);
        active_.erase(found);
        return emitFrame(builder, error);
    }
    return true;
}

bool PictureAssembler::finalizeAll(std::string& error) {
    while (!order_.empty()) {
        if (!finalizeOldest(error)) {
            return false;
        }
    }
    active_.clear();
    order_.clear();
    return true;
}

void PictureAssembler::checkSliceCoverage(const std::vector<SliceMeta>& slices,
                                          uint64_t picture_mbs, uint64_t frame_offset,
                                          bool& incomplete, uint32_t& missing) const {
    if (slices.empty()) {
        incomplete = true;
        ++missing;
        return;
    }

    std::vector<uint32_t> addresses;
    addresses.reserve(slices.size());
    for (const SliceMeta& slice : slices) {
        addresses.push_back(slice.first_mb_in_slice);
    }
    std::sort(addresses.begin(), addresses.end());
    if (addresses.front() != 0U) {
        incomplete = true;
        ++missing;
        warning_callback_("incomplete frame at offset " + std::to_string(frame_offset) +
                          ": first slice does not start at macroblock 0");
    }

    std::set<uint32_t> unique;
    for (uint32_t address : addresses) {
        if (!unique.insert(address).second) {
            warning_callback_("duplicate slice first_mb_in_slice=" +
                              std::to_string(address) + " at offset " +
                              std::to_string(frame_offset));
        }
    }
    if (!addresses.empty() && addresses.back() >= picture_mbs) {
        incomplete = true;
        ++missing;
        warning_callback_("incomplete frame at offset " + std::to_string(frame_offset) +
                          ": slice macroblock address exceeds picture size");
    }
}

bool PictureAssembler::emitFrame(FrameBuilder& builder, std::string& error) {
    Frame frame;
    frame.idr = builder.idr;
    frame.pps_id = builder.pps_id;
    frame.frame_num = builder.frame_num;
    frame.poc_lsb = builder.poc_lsb;
    frame.field_pic = builder.field_pic;
    frame.start_offset = builder.slices.front().start_offset;
    frame.end_offset = builder.slices.front().end_offset;

    std::array<uint32_t, 3> type_counts{{0, 0, 0}};
    std::vector<SliceMeta> top_slices;
    std::vector<SliceMeta> bottom_slices;
    std::vector<SliceMeta> progressive_slices;
    for (const SliceMeta& slice : builder.slices) {
        frame.start_offset = std::min(frame.start_offset, slice.start_offset);
        frame.end_offset = std::max(frame.end_offset, slice.end_offset);
        ++frame.slice_count;
        ++type_counts[normalizeType(slice.slice_type)];
        if (!slice.field_pic) {
            progressive_slices.push_back(slice);
        } else if (slice.bottom_field) {
            bottom_slices.push_back(slice);
        } else {
            top_slices.push_back(slice);
        }
    }

    if (type_counts[2] >= type_counts[1] && type_counts[2] >= type_counts[0]) {
        frame.slice_type = 2U;
    } else if (type_counts[0] >= type_counts[1]) {
        frame.slice_type = 0U;
    } else {
        frame.slice_type = 1U;
    }

    if (builder.field_pic) {
        frame.has_top_field = !top_slices.empty();
        frame.has_bottom_field = !bottom_slices.empty();
        if (frame.has_top_field && frame.has_bottom_field) {
            frame.field_pairs = 1U;
        } else {
            frame.incomplete = true;
            ++frame.missing_slices;
            warning_callback_("incomplete field pair at offset " +
                              std::to_string(frame.start_offset) + ": missing " +
                              (frame.has_top_field ? "bottom" : "top") + " field");
        }
        const uint64_t field_mbs = static_cast<uint64_t>(sps_.pic_width_in_mbs_minus1 + 1U) *
                                   (sps_.pic_height_in_map_units_minus1 + 1U);
        checkSliceCoverage(top_slices, field_mbs, frame.start_offset,
                           frame.incomplete, frame.missing_slices);
        checkSliceCoverage(bottom_slices, field_mbs, frame.start_offset,
                           frame.incomplete, frame.missing_slices);
    } else {
        const uint64_t picture_mbs = static_cast<uint64_t>(sps_.pic_width_in_mbs_minus1 + 1U) *
                                     (sps_.pic_height_in_map_units_minus1 + 1U) *
                                     (sps_.frame_mbs_only_flag ? 1U : 2U);
        checkSliceCoverage(progressive_slices, picture_mbs, frame.start_offset,
                           frame.incomplete, frame.missing_slices);
    }

    if (!frame_callback_(frame)) {
        error = "frame callback stopped assembly";
        return false;
    }
    return true;
}

}  // namespace h264scan
