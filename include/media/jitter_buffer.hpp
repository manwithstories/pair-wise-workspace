// 抖动缓冲：依据滑动窗口吞吐与到达抖动自适应调整缓冲深度。
// 带宽突降时保留可恢复数据（不主动丢弃未提交包），恢复后限制等待时间。
#pragma once

#include <cstdint>
#include <deque>

namespace media {

struct JitterSnapshot {
    double throughput_kbps = 0.0; // 滑动窗口吞吐
    double jitter_ms = 0.0;       // 到达间隔抖动（EWMA）
    uint32_t target_depth = 0;    // 建议缓冲深度（包数）
    bool bandwidth_drop = false;  // 吞吐较长期均值突降
};

class JitterBuffer {
  public:
    // window_ms: 吞吐统计滑动窗口；max_wait_ms: 恢复后最大等待。
    JitterBuffer(int64_t window_ms, uint32_t min_depth, uint32_t max_depth,
                 int64_t max_wait_ms)
        : window_ms_(window_ms), min_depth_(min_depth), max_depth_(max_depth),
          max_wait_ms_(max_wait_ms) {}

    // 记录一次到达，返回最新统计快照。
    JitterSnapshot observe(int64_t arrival_ms, uint32_t payload_bytes);

    // 带宽突降期间：保留可恢复数据，不收缩到 min_depth 以下。
    bool should_retain_recoverable() const { return bandwidth_drop_; }

    // 恢复完成后的等待上限。
    int64_t max_wait_ms() const { return max_wait_ms_; }

    uint32_t target_depth() const { return target_depth_; }

  private:
    int64_t window_ms_;
    uint32_t min_depth_;
    uint32_t max_depth_;
    int64_t max_wait_ms_;

    std::deque<std::pair<int64_t, uint32_t>> arrivals_; // (到达ms, 字节)
    int64_t last_arrival_ms_ = -1;
    double jitter_ms_ = 0.0;
    double long_term_kbps_ = 0.0; // 长期吞吐 EWMA
    uint32_t target_depth_ = 0;
    bool bandwidth_drop_ = false;
};

} // namespace media
