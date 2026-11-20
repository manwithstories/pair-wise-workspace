// mediacore/quality.hpp — 样本质量与码率统计
//
// 统计只针对「有效时间范围」内的样本，与 manifest 共享同一
// SampleRange（由流水线在跳过 Corrupt/Gap/Duplicate 后汇总给出）。
// 全程不解码/重编码：输入为原始 s16 PCM，直接按样本统计。
#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "mediacore/pcm.hpp"
#include "mediacore/time.hpp"

namespace mediacore {

struct QualityStats {
    // 有效样本范围（采样刻度，半开区间）
    SampleRange validRange{};
    SampleTick discontinuities = 0;   // 连续性断点计数（缺口/损坏/重复边界）
    std::uint64_t totalSamples = 0;   // 有效样本数（按帧 * 声道）
    std::uint64_t clippedSamples = 0; // 触达满幅 ±32767/−32768 的样本
    std::int16_t peakAbs = 0;         // 最大绝对值
    double rms = 0.0;                 // 有效样本均方根
    double durationSec = 0.0;

    // 字节与码率（原始=输入声明字节；实际=写入容器字节）
    std::uint64_t rawBytes = 0;
    std::uint64_t actualBytes = 0;
    int targetKbps = 0;
    double actualKbps = 0.0;
    double bitrateErrorPct = 0.0;

    // 终端量化摘要单行
    std::string summaryLine() const;
};

class QualityAnalyzer {
public:
    explicit QualityAnalyzer(int sampleRate);

    // 登记一个被流水线接受（有效）的 PCM 块；expectedStart 为连续性期望值
    void addValidBlock(const AudioBlock& block, SampleTick expectedStart);

    // 登记跳过区间（损坏/缺口/重复），用于连续性断点计数
    void addSkippedRange(SampleRange range);

    // 汇总：validRange 与 manifest 同源；actualBytes/目标码率由调用方填入
    QualityStats finalize(SampleRange validRange,
                          std::uint64_t rawBytes,
                          std::uint64_t actualBytes,
                          int targetKbps) const;

private:
    int sampleRate_ = 0;
    std::uint64_t samples_ = 0;
    std::uint64_t clipped_ = 0;
    std::uint64_t sumSq_ = 0; // 样本平方和（s16 平方不超过 2^30，uint64 足够）
    std::int32_t peak_ = 0;
    SampleTick discontinuities_ = 0;
    std::vector<SampleRange> skipped_;
};

} // namespace mediacore
