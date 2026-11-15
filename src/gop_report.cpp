#include "gop_report.h"

#include <algorithm>
#include <utility>

namespace h264scan {
namespace {

constexpr size_t kMaxStoredWarnings = 200U;
constexpr size_t kRecentFrameLimit = 256U;

}  // namespace

GopReporter::GopReporter(std::string file_name) {
    report_.file = std::move(file_name);
}

void GopReporter::addWarning(const std::string& message) {
    ++report_.warning_count;
    if (report_.warnings.size() < kMaxStoredWarnings) {
        report_.warnings.push_back(message);
    }
}

bool GopReporter::isPocFallback(const Frame& frame) const {
    if (!gop_open_ || frame.idr || frame.poc_lsb >= last_poc_lsb_) {
        return false;
    }
    if (frame.frame_num < last_frame_num_ ||
        (frame.frame_num == 0U && last_frame_num_ != 0U)) {
        return true;
    }
    const uint32_t poc_bits = report_.sps.pic_order_cnt_lsb_bits();
    const uint32_t max_poc = poc_bits >= 31U
                                 ? 0x7fffffffU
                                 : ((uint32_t{1} << poc_bits) - 1U);
    const uint32_t drop = last_poc_lsb_ - frame.poc_lsb;
    return drop > max_poc / 2U;
}

bool GopReporter::beginGop(const Frame& frame) {
    current_ = GopStats{};
    current_.index = static_cast<uint32_t>(report_.gops.size());
    current_.start_offset = frame.start_offset;
    gop_warning_start_ = report_.warning_count;
    last_end_offset_ = frame.end_offset;
    last_poc_lsb_ = frame.poc_lsb;
    last_frame_num_ = frame.frame_num;
    gop_open_ = true;
    max_slices_per_frame_ = 0;
    recent_frames_.clear();
    return true;
}

void GopReporter::applySliceDeficit(RecentFrame& recent) {
    if (max_slices_per_frame_ == 0U ||
        recent.frame.slice_count >= max_slices_per_frame_) {
        return;
    }
    const uint32_t deficit = static_cast<uint32_t>(
        max_slices_per_frame_ - recent.frame.slice_count);
    const uint32_t desired = std::max(recent.frame.missing_slices, deficit);
    if (desired <= recent.applied_missing) {
        return;
    }

    current_.missing_slices += desired - recent.applied_missing;
    recent.applied_missing = desired;
    if (!recent.frame.incomplete) {
        recent.frame.incomplete = true;
        ++current_.incomplete_frames;
    }
    if (!recent.deficit_warned) {
        recent.deficit_warned = true;
        addWarning("incomplete frame at offset " +
                   std::to_string(recent.frame.start_offset) + ": expected " +
                   std::to_string(max_slices_per_frame_) + " slices, found " +
                   std::to_string(recent.frame.slice_count));
    }
}

bool GopReporter::addFrame(const Frame& source, std::string& error) {
    if (gop_open_ && (source.idr || isPocFallback(source))) {
        if (!finalizeGop(error)) {
            return false;
        }
    }
    if (!gop_open_ && !beginGop(source)) {
        error = "failed to begin GOP";
        return false;
    }

    const Frame& frame = source;

    ++current_.frames;
    current_.slice_count += frame.slice_count;
    current_.field_pairs += frame.field_pairs;
    current_.missing_slices += frame.missing_slices;
    if (frame.incomplete) {
        ++current_.incomplete_frames;
    }
    RecentFrame recent;
    recent.frame = frame;
    recent.applied_missing = frame.missing_slices;
    recent_frames_.push_back(recent);
    if (frame.slice_count > max_slices_per_frame_) {
        max_slices_per_frame_ = frame.slice_count;
        for (RecentFrame& item : recent_frames_) {
            applySliceDeficit(item);
        }
    } else {
        applySliceDeficit(recent_frames_.back());
    }
    while (recent_frames_.size() > kRecentFrameLimit) {
        recent_frames_.pop_front();
    }
    if (frame.slice_type == 2U) {
        ++current_.i_frames;
    } else if (frame.slice_type == 1U) {
        ++current_.b_frames;
    } else {
        ++current_.p_frames;
    }
    last_end_offset_ = std::max(last_end_offset_, frame.end_offset);
    last_poc_lsb_ = frame.poc_lsb;
    last_frame_num_ = frame.frame_num;
    return true;
}

bool GopReporter::finalizeGop(std::string& error) {
    if (!gop_open_) {
        return true;
    }
    current_.byte_length = last_end_offset_ >= current_.start_offset
                               ? last_end_offset_ - current_.start_offset
                               : 0U;
    current_.warnings = report_.warning_count - gop_warning_start_;
    report_.gops.push_back(current_);
    gop_open_ = false;
    recent_frames_.clear();
    (void)error;
    return true;
}

bool GopReporter::finish(std::string& error) {
    if (!finalizeGop(error)) {
        return false;
    }
    recomputeTotals();
    return true;
}

void GopReporter::recomputeTotals() {
    ReportTotals totals;
    totals.gop_count = report_.gops.size();
    for (const GopStats& gop : report_.gops) {
        totals.frames += gop.frames;
        totals.slice_count += gop.slice_count;
        totals.i_frames += gop.i_frames;
        totals.p_frames += gop.p_frames;
        totals.b_frames += gop.b_frames;
        totals.field_pairs += gop.field_pairs;
        totals.missing_slices += gop.missing_slices;
        totals.incomplete_frames += gop.incomplete_frames;
    }
    totals.warnings = report_.warning_count;
    report_.totals = totals;
}

}  // namespace h264scan
