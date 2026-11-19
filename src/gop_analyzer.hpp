#pragma once
// 关键帧间隔（GOP）统计与异常跳变检测，并累计 VFR 帧间隔分布。
#include "media_clock.hpp"
#include "media_types.hpp"

#include <vector>

namespace ma {

struct GopReport {
    int keyframeCount = 0;
    int anomalies = 0;                 // 显著偏离中位间隔的 GOP
    double intervalMeanMs = 0.0;
    double intervalP95Ms = 0.0;
    std::vector<double> frameIntervalsMs; // 全帧间隔（VFR 分布原始样本）
};

// 每个视频流一个实例；按解码顺序喂入包信息。
class GopAnalyzer {
public:
    GopAnalyzer(int tbNum, int tbDen) : tbNum_(tbNum), tbDen_(tbDen) {}

    void feed(const PacketInfo& pkt) {
        if (pkt.streamIndex < 0) return;
        ++frameSeen_;
        if (!lastPtsValid_) {
            lastPts_ = pkt.pts;
            lastPtsValid_ = true;
            if (pkt.keyframe) handleKey(pkt);
            return;
        }
        const double gapMs = StreamClock::toMs(
            pkt.pts - lastPts_, tbNum_, tbDen_);
        if (gapMs > 0.0) frameIntervalsMs_.push_back(gapMs);
        lastPts_ = pkt.pts;
        if (pkt.keyframe) handleKey(pkt);
    }

    GopReport report() const {
        GopReport r;
        r.keyframeCount = keyCount_;
        r.anomalies = anomalies_;
        auto frames = frameIntervalsMs_;
        r.frameIntervalsMs = frameIntervalsMs_;
        if (!gopIntervalsMs_.empty()) {
            double sum = 0.0;
            for (double v : gopIntervalsMs_) sum += v;
            r.intervalMeanMs = sum /
                static_cast<double>(gopIntervalsMs_.size());
            auto v = gopIntervalsMs_;
            r.intervalP95Ms = percentileMs(v, 95.0);
        }
        return r;
    }

private:
    void handleKey(const PacketInfo& pkt) {
        ++keyCount_;
        if (lastKeyValid_) {
            const double gopMs = StreamClock::toMs(
                pkt.pts - lastKeyPts_, tbNum_, tbDen_);
            if (gopMs > 0.0) {
                gopIntervalsMs_.push_back(gopMs);
                // 异常：超过当前中位 GOP 的 2 倍（首个 GOP 仅建立基线）。
                if (!gopIntervalsMs_.empty() && gopIntervalsMs_.size() >= 2) {
                    auto sorted = gopIntervalsMs_;
                    double median = percentileMs(sorted, 50.0);
                    if (median > 0.0 && gopMs > 2.0 * median) ++anomalies_;
                }
            }
        }
        lastKeyPts_ = pkt.pts;
        lastKeyValid_ = true;
    }

    int tbNum_, tbDen_;
    int frameSeen_ = 0;
    int keyCount_ = 0;
    int anomalies_ = 0;
    int64_t lastPts_ = 0;
    int64_t lastKeyPts_ = 0;
    bool lastPtsValid_ = false;
    bool lastKeyValid_ = false;
    std::vector<double> frameIntervalsMs_;
    std::vector<double> gopIntervalsMs_;
};

} // namespace ma
