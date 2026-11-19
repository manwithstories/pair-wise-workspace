#include "sync_recovery.hpp"
#include "media_clock.hpp"

#include <cmath>

namespace ma {

bool SyncRecovery::interpolate(const std::vector<Point>& s, double t,
                               double& out) {
    if (s.empty()) return false;
    if (t <= s.front().t) { out = s.front().actual; return true; }
    if (t >= s.back().t) { out = s.back().actual; return true; }
    for (size_t i = 1; i < s.size(); ++i) {
        if (t <= s[i].t) {
            const double span = s[i].t - s[i - 1].t;
            const double f = span > 0.0 ? (t - s[i - 1].t) / span : 0.0;
            out = s[i - 1].actual + f * (s[i].actual - s[i - 1].actual);
            return true;
        }
    }
    return false;
}

double SyncRecovery::rms(const std::vector<double>& v) {
    if (v.empty()) return 0.0;
    double sum = 0.0;
    for (double x : v) sum += x * x;
    return std::sqrt(sum / static_cast<double>(v.size()));
}

SyncReport SyncRecovery::estimate() const {
    SyncReport rep;
    if (video_.empty() || audio_.empty()) return rep;

    // 对每个视频理想时刻 t，插值音频实际时间；
    // 音画偏移 = 视频实际 - 音频实际（视频超前为正）。
    struct Pair { double t; double offset; };
    std::vector<Pair> pairs;
    for (const auto& vp : video_) {
        double audioActual = 0.0;
        if (!interpolate(audio_, vp.t, audioActual)) continue;
        pairs.push_back({vp.t, vp.actual - audioActual});
    }
    if (pairs.size() < 2) return rep;
    rep.sampleCount = static_cast<int>(pairs.size());
    for (const auto& p : pairs) rep.rawOffsetsMs.push_back(p.offset);

    // 最小二乘拟合 offset(t) = b + k*t（k 单位 ms/ms）。
    const double n = static_cast<double>(pairs.size());
    double st = 0.0, so = 0.0;
    for (const auto& p : pairs) { st += p.t; so += p.offset; }
    const double mt = st / n, mo = so / n;
    double sxx = 0.0, sxy = 0.0;
    for (const auto& p : pairs) {
        sxx += (p.t - mt) * (p.t - mt);
        sxy += (p.t - mt) * (p.offset - mo);
    }
    const double k = sxx > 1e-12 ? sxy / sxx : 0.0;
    const double b = mo - k * mt;
    rep.initialOffsetMs = b + k * pairs.front().t;
    rep.driftPpm = k * 1e6;

    // 校正只生成残差序列；原始偏移原样保留，不被覆盖。
    std::vector<double> beforeAbs, afterAbs;
    for (const auto& p : pairs) {
        const double residual = p.offset - (b + k * p.t);
        rep.residualsMs.push_back(residual);
        beforeAbs.push_back(std::fabs(p.offset));
        afterAbs.push_back(std::fabs(residual));
    }
    rep.errorBeforeRmsMs = rms(rep.rawOffsetsMs);
    rep.errorAfterRmsMs = rms(rep.residualsMs);
    rep.errorBeforeP95Ms = percentileMs(beforeAbs, 95.0);
    rep.errorAfterP95Ms = percentileMs(afterAbs, 95.0);

    // 异常点：残差超过 3 倍残差 RMS（校正不掩盖原始异常）。
    const double thr = 3.0 * rep.errorAfterRmsMs;
    if (thr > 0.0)
        for (double r : rep.residualsMs)
            if (std::fabs(r) > thr) ++rep.anomalyCount;
    return rep;
}

} // namespace ma
