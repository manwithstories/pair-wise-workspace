#pragma once
// 分析模块间传递的轻量数据包/帧信息（不持有 AVPacket 所有权）。
#include <cstdint>
#include <vector>

namespace ma {

struct PacketInfo {
    int streamIndex = 0;
    int64_t pts = 0;            // 原始 PTS（AV_NOPTS_VALUE 归一为 0）
    int64_t dts = 0;
    int64_t duration = 0;
    int64_t pos = -1;           // 文件字节偏移
    int size = 0;
    bool keyframe = false;
    bool hasPts = false;
};

struct FrameInfo {
    int streamIndex = 0;
    int64_t pts = 0;
    bool keyframe = false;
    bool audio = false;
    int samples = 0;
};

// 帧间隔分布统计。
struct IntervalStats {
    int count = 0;
    double minMs = 0.0;
    double maxMs = 0.0;
    double meanMs = 0.0;
    double p95Ms = 0.0;
    void finalize(std::vector<double>& samples);
};

} // namespace ma
