// segmenter.hpp — 关键帧索引 + 静音阈值切片，明确处置结果
#pragma once
#include "common.hpp"
#include <functional>
#include <vector>

namespace md {

struct SegmenterCounters {
    uint32_t missing_keyframe = 0;
    uint32_t short_segments = 0;
    uint32_t silence_cross = 0;
    uint32_t keyframe_total = 0;
    uint32_t keyframe_indexed = 0;
};

// 闭合的片段：有界缓存（max_frame_cache），峰值内存不随输入时长增长
struct ClosedSegment {
    std::vector<MediaPacket> packets;
    bool starts_with_keyframe = false;
    bool silence_cross_boundary = false;
    Disposition disposition = Disposition::Normal;
};

class Segmenter {
public:
    explicit Segmenter(const ProcessConfig& cfg) : cfg_(cfg) {}

    // 喂入一个（已归一化的）视频包；片段闭合时通过 emit 回调交付
    void push_video(const MediaPacket& p,
                    const std::vector<SilenceInterval>& silence,
                    std::function<void(ClosedSegment)> emit);

    // 输入结束，闭合最后片段（可能短于最小时长）
    void finish(const std::vector<SilenceInterval>& silence,
                std::function<void(ClosedSegment)> emit);

    const SegmenterCounters& counters() const { return counters_; }
    void reset() {
        cur_.clear(); cur_indexed_ = false; cur_silence_cross_ = false;
        counters_ = {};
    }

    // H.264 NAL 关键帧判定（SPS/PPS/IDR），供测试与流水线共用
    static bool h264_is_keyframe(const std::vector<uint8_t>& pl);

private:
    bool current_silence_crosses(const MediaPacket& p,
                                 const std::vector<SilenceInterval>& s) const;
    void close_segment(std::function<void(ClosedSegment)> emit);

    const ProcessConfig& cfg_;
    std::vector<MediaPacket> cur_;
    bool cur_indexed_ = false;
    bool cur_silence_cross_ = false;
    SegmenterCounters counters_;
};

} // namespace md
