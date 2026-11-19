#include "media/jitter_buffer.hpp"

#include <algorithm>
#include <cstdlib>

namespace media {

JitterSnapshot JitterBuffer::observe(int64_t arrival_ms, uint32_t payload_bytes) {
    // 到达抖动：相邻到达间隔偏差的 EWMA（RFC 3550 形式）。
    if (last_arrival_ms_ >= 0) {
        double transit = static_cast<double>(arrival_ms - last_arrival_ms_);
        double d = std::abs(transit - 8.0); // 8ms 为标称包间隔基准
        jitter_ms_ += (d - jitter_ms_) / 16.0;
    }
    last_arrival_ms_ = arrival_ms;

    arrivals_.emplace_back(arrival_ms, payload_bytes);
    while (!arrivals_.empty() &&
           arrival_ms - arrivals_.front().first > window_ms_)
        arrivals_.pop_front();

    // 滑动窗口吞吐。
    double window_kbps = 0.0;
    if (arrivals_.size() >= 2) {
        int64_t span_ms = arrivals_.back().first - arrivals_.front().first;
        if (span_ms > 0) {
            uint64_t bytes = 0;
            for (const auto& [ts, b] : arrivals_) bytes += b;
            window_kbps = static_cast<double>(bytes) * 8.0 / span_ms; // kbps
        }
    }

    // 长期均值与突降判定：窗口吞吐低于长期均值一半视为带宽突降。
    if (long_term_kbps_ <= 0.0) {
        long_term_kbps_ = window_kbps;
    } else if (window_kbps > 0.0) {
        bandwidth_drop_ = window_kbps < long_term_kbps_ * 0.5;
        // 突降期间冻结长期均值，避免被低吞吐自我拉低。
        if (!bandwidth_drop_)
            long_term_kbps_ += (window_kbps - long_term_kbps_) / 32.0;
    }

    // 目标深度：基准深度 + 抖动吸收量；突降时保持深度以保留可恢复数据。
    double jitter_packets = jitter_ms_ / 8.0;
    uint32_t adaptive =
        min_depth_ + static_cast<uint32_t>(jitter_packets * 2.0 + 0.5);
    if (bandwidth_drop_)
        adaptive = std::max(adaptive, target_depth_); // 只增不减
    target_depth_ = std::clamp(adaptive, min_depth_, max_depth_);

    return {window_kbps, jitter_ms_, target_depth_, bandwidth_drop_};
}

} // namespace media
