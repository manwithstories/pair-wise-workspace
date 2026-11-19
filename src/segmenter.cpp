#include "segmenter.hpp"
#include <algorithm>

namespace md {
namespace {
// 跳过 00 00 01 / 00 00 00 01 起始码，返回首个 NAL 头位置
size_t skip_start_code(const std::vector<uint8_t>& pl) {
    size_t i = 0;
    if (pl.size() >= 4 && pl[0] == 0 && pl[1] == 0 && pl[2] == 0 && pl[3] == 1)
        i = 4;
    else if (pl.size() >= 3 && pl[0] == 0 && pl[1] == 0 && pl[2] == 1)
        i = 3;
    return i;
}
} // namespace

bool Segmenter::h264_is_keyframe(const std::vector<uint8_t>& pl) {
    if (pl.empty()) return false;
    size_t i = skip_start_code(pl);
    if (i >= pl.size()) return false;
    unsigned nal_type = pl[i] & 0x1F;
    // 5=IDR, 7=SPS, 8=PPS 均视为关键帧索引点
    return nal_type == 5 || nal_type == 7 || nal_type == 8;
}

bool Segmenter::current_silence_crosses(
        const MediaPacket& p, const std::vector<SilenceInterval>& s) const {
    // 静音区间同时包含当前包之前与之后的时间 => 跨越包/片段边界
    for (const auto& iv : s) {
        if (iv.begin_us < p.unified_us && iv.end_us > p.unified_us)
            return true;
    }
    return false;
}

void Segmenter::close_segment(std::function<void(ClosedSegment)> emit) {
    if (cur_.empty()) return;
    ClosedSegment seg;
    seg.packets = std::move(cur_);
    cur_.clear();
    seg.starts_with_keyframe = cur_indexed_;
    seg.silence_cross_boundary = cur_silence_cross_;

    int64_t duration = seg.packets.back().unified_us -
                       seg.packets.front().unified_us;
    // 处置优先级：缺关键帧 > 静音跨边界 > 片段过短
    if (!seg.starts_with_keyframe) {
        seg.disposition = Disposition::MissingKeyframe;
        ++counters_.missing_keyframe;
    } else if (seg.silence_cross_boundary) {
        seg.disposition = Disposition::SilenceCrossBoundary;
        ++counters_.silence_cross;
    } else if (duration < cfg_.min_segment_us) {
        seg.disposition = Disposition::ShortSegment;
        ++counters_.short_segments;
    }
    if (seg.packets.front().untrusted)
        seg.disposition = Disposition::UntrustedTime;

    cur_indexed_ = false;
    cur_silence_cross_ = false;
    emit(std::move(seg));
}

void Segmenter::push_video(const MediaPacket& p,
                           const std::vector<SilenceInterval>& silence,
                           std::function<void(ClosedSegment)> emit) {
    bool kf = h264_is_keyframe(p.payload);
    if (kf) ++counters_.keyframe_total;

    // 切片规则：新关键帧到达且当前片段已满足最小时长 => 先闭合旧片段
    if (kf && !cur_.empty()) {
        int64_t dur = p.unified_us - cur_.front().unified_us;
        if (dur >= cfg_.min_segment_us)
            close_segment(emit);
    }

    if (cur_.empty()) {
        cur_indexed_ = kf;
        if (kf) ++counters_.keyframe_indexed;
    }
    if (current_silence_crosses(p, silence))
        cur_silence_cross_ = true;

    cur_.push_back(p);

    // 有界帧缓存：超过上限强制闭合，保证内存不随输入时长增长
    if (cur_.size() >= cfg_.max_frame_cache)
        close_segment(emit);
}

void Segmenter::finish(const std::vector<SilenceInterval>& silence,
                       std::function<void(ClosedSegment)> emit) {
    (void)silence;
    close_segment(emit);
}

} // namespace md
