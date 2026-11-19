#pragma once
// 音视频漂移联合估计：在公共理想时间轴上配对两条流的实际呈现时间，
// 线性回归拟合漂移，记录校正前后同步误差。
// 原始观测全部保留——校正只产生残差序列，不修改原始数据。
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

// 喂入 (理想参考时间 t, 该流实际呈现时间 actual)，单位 ms。
// 两条流共用同一理想时间轴（帧/样本序号换算），配对在其上完成。
class SyncRecovery {
public:
    void addVideo(double idealMs, double actualMs) {
        video_.push_back({idealMs, actualMs});
    }
    void addAudio(double idealMs, double actualMs) {
        audio_.push_back({idealMs, actualMs});
    }

    SyncReport estimate() const;

private:
    struct Point { double t; double actual; };
    static bool interpolate(const std::vector<Point>& s, double t,
                            double& out);
    static double rms(const std::vector<double>& v);

    std::vector<Point> video_, audio_;
};

} // namespace ma
