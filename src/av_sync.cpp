#include "media/av_sync.hpp"

#include "media/timebase.hpp"

#include <cmath>

extern "C" {
#include <libswresample/swresample.h>
}

namespace media {

void AvSync::add_pair(int64_t audio_pts_ticks, int64_t video_pts_ticks) {
    pairs_.push_back({audio_pts_ticks, video_pts_ticks});
}

AvSyncReport AvSync::report() const {
    AvSyncReport rep;
    rep.sample_pairs = pairs_.size();
    if (pairs_.empty()) return rep;

    // 对偏差序列 d(t) = audio - video 做一元线性回归：
    // d(t) = offset + drift * t。自变量用视频 PTS 的毫秒值。
    double n = static_cast<double>(pairs_.size());
    double sx = 0, sy = 0, sxx = 0, sxy = 0;
    for (const auto& p : pairs_) {
        double x = ticks_to_ms(p.video);
        double y = ticks_to_ms(p.audio - p.video);
        sx += x;
        sy += y;
        sxx += x * x;
        sxy += x * y;
    }
    double denom = n * sxx - sx * sx;
    double slope = 0.0; // ms 偏差 / ms 时间
    double intercept = sy / n;
    if (std::abs(denom) > 1e-9) {
        slope = (n * sxy - sx * sy) / denom;
        intercept = (sy - slope * sx) / n;
    }
    rep.offset_ms = intercept;
    rep.drift_ppm = slope * 1e6;

    double max_resid = 0.0;
    for (const auto& p : pairs_) {
        double x = ticks_to_ms(p.video);
        double y = ticks_to_ms(p.audio - p.video);
        max_resid = std::max(max_resid, std::abs(y - (intercept + slope * x)));
    }
    rep.residual_error_ms = max_resid;
    return rep;
}

std::vector<int16_t> resample_s16_mono(const std::vector<int16_t>& input,
                                       int src_rate, int dst_rate) {
    if (input.empty() || src_rate <= 0 || dst_rate <= 0) return {};

    SwrContext* swr = nullptr;
    AVChannelLayout layout;
    av_channel_layout_default(&layout, 1);
    int ret = swr_alloc_set_opts2(&swr, &layout, AV_SAMPLE_FMT_S16, dst_rate,
                                  &layout, AV_SAMPLE_FMT_S16, src_rate, 0,
                                  nullptr);
    av_channel_layout_uninit(&layout);
    if (ret < 0 || !swr) return {};
    if (swr_init(swr) < 0) {
        swr_free(&swr);
        return {};
    }

    int64_t out_samples =
        av_rescale_rnd(static_cast<int64_t>(input.size()), dst_rate, src_rate,
                       AV_ROUND_UP) +
        256;
    std::vector<int16_t> output(static_cast<size_t>(out_samples));
    const uint8_t* in_data =
        reinterpret_cast<const uint8_t*>(input.data());
    uint8_t* out_data = reinterpret_cast<uint8_t*>(output.data());
    int converted =
        swr_convert(swr, &out_data, static_cast<int>(out_samples), &in_data,
                    static_cast<int>(input.size()));
    // 冲刷重采样器内部延迟。
    if (converted >= 0) {
        int flushed = swr_convert(swr, &out_data, static_cast<int>(out_samples),
                                  nullptr, 0);
        if (flushed > 0) converted += flushed;
    }
    swr_free(&swr);
    if (converted < 0) return {};
    output.resize(static_cast<size_t>(converted));
    return output;
}

} // namespace media
