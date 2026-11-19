// segmenter.hpp — 关键帧索引 + 静音阈值切片，明确处置结果
#pragma once
#include "common.hpp"
#include <functional>
#include <vector>

namespace md {

// 切分阶段输入：归一化包 + 当前已知静音区间（来自静音阶段）
struct SegmenterCounters {
    uint32_t missing_keyframe = 0;
    uint32_t short_segments = 0;
    uint32_t silence_cross = 0;
    uint32_t keyframe_total = 0;
    uint32_t keyframe_indexed = 0;
};

// 一个进行中的片段：缓存有上限（max_frame_cache），不随时长增长
class Segmenter {
public:
    explicit Segmenter(const ProcessConfig& cfg) : cfg_(cfg) {}

    // 喂入视频包；片段闭合时回调 emit（携带片段包集合与处置）
    void push_video(const MediaPacket& p,
                    const std::vector<SilenceInterval>& silence,
                    std::function<void(std::vector<MediaPacket>, Disposition)> emit);

    // 输入结束，闭合最后片段
    void finish(const std::vector<SilenceInterval>& silence,
                std::function<void(std::vector<MediaPacket>, Disposition)> emit);

    const SegmenterCounters& counters() const { return counters_; }
    void reset() { cur_.clear(); cur_indexed_ = false; counters_ = {}; }

private:
    bool is_keyframe(const MediaPacket& p) const;
    bool silence_long_enough(int64_t begin, int64_t end) const;

    const ProcessConfig& cfg_;
    std::vector<MediaPacket> cur_;
    bool cur_indexed_ = false;
    SegmenterCounters counters_;
};

} // namespace md
