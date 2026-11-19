// 音画同步：以统一时间基准处理音视频偏移、漂移与重采样。
// 偏移/漂移由成对音视 PTS 最小二乘估计；音频经 libswresample 重采样。
#pragma once

#include <cstdint>
#include <vector>

namespace media {

struct AvSyncReport {
    double offset_ms = 0.0;         // 估计的音画偏移（音频 - 视频）
    double drift_ppm = 0.0;         // 时钟漂移（百万分之一）
    double residual_error_ms = 0.0; // 线性校正后的最大残差
    uint64_t sample_pairs = 0;
};

// 累积成对的音频/视频 PTS（主时基 tick），输出偏移与漂移估计。
class AvSync {
  public:
    void add_pair(int64_t audio_pts_ticks, int64_t video_pts_ticks);
    AvSyncReport report() const;

  private:
    struct Pair {
        int64_t audio;
        int64_t video;
    };
    std::vector<Pair> pairs_; // 30 分钟 @ 每秒一对约 1800 项，内存可忽略
};

// 单声道 s16 重采样（libswresample）。失败返回空向量。
std::vector<int16_t> resample_s16_mono(const std::vector<int16_t>& input,
                                       int src_rate, int dst_rate);

} // namespace media
