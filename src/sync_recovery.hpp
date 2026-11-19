#pragma once
// 音视频漂移联合估计：线性回归拟合漂移，记录校正前后同步误差。
// 原始偏移与异常点全部保留——校正只产生残差序列，不修改原始观测。
#include <vector>

namespace ma {

struct SyncReport {
    int sampleCount = 0;
    double initialOffsetMs = 0.0; // 截距：起点处音画偏移
    double driftPpm = 0.0;        // 斜率换算的相对漂移
    double errorBeforeRmsMs = 0.0;
    double errorAfterRmsMs = 0.0;
    double errorBeforeP95Ms = 0.0;
    double errorAfterP95Ms = 0.0;
    int anomalyCount = 0;         // 偏移跳变异常点（校正后仍可见）
    std::vector<double> rawOffsetsMs;  // 原始音画偏移（供复核）
    std::vector<double> residualsMs;   // 去漂移后的残差
};

// 每条流按自身期望时钟喂点，内部在公共参考时间上配对。
class SyncRecovery {
public:
    // ptsMs：展开后的实际呈现时间；expectedMs：按帧序号/采样数推算的理想时间
    void addVideo(double ptsMs, double expectedMs) {
        video_.push_back({expectedMs, ptsMs - expectedMs});
    }
    void addAudio(double ptsMs, double expectedMs) {
        audio_.push_back({expectedMs, ptsMs - expectedMs});
    }

    SyncReport estimate() const;

private:
    struct Point {
        double t;      // 公共参考时间（ms）
        double offset; // 相对理想时钟的偏移（ms）
    };
    // 在参考时间 t 处线性插值流偏移；失败返回 false。
    static bool interpolate(const std::vector<Point>& s, double t,
                            double& out);
    static double rms(const std::vector<double>& v);

    std::vector<Point> video_, audio_;
};

} // namespace ma
