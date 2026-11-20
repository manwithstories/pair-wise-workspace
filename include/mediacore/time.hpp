// mediacore/time.hpp — 统一媒体时间基准
//
// 全系统以「采样刻度 (sample tick)」作为唯一时间表示：任何时间戳、区间、
// 持续时间都以单声道样本帧为单位计数，与封装层的 packet PTS 解耦。
// 输出封装时再由 muxer 换算成对应时基的 PTS。
#pragma once

#include <cstdint>
#include <string>

namespace mediacore {

// 单声道采样帧序号 / 时间戳，单位：samples
using SampleTick = std::int64_t;

// 字节偏移，单位：bytes
using ByteOffset = std::int64_t;

// 统一的有效/跳过时间范围（均为采样刻度，半开区间 [start, end)）
struct SampleRange {
    SampleTick start = 0;
    SampleTick end   = 0;

    SampleTick length() const noexcept { return end - start; }
    bool empty() const noexcept { return end <= start; }

    bool operator==(const SampleRange& other) const noexcept {
        return start == other.start && end == other.end;
    }
};

// 采样刻度 <-> 秒 / 毫秒
constexpr double samplesToSeconds(SampleTick samples, int sampleRate) noexcept {
    return sampleRate > 0 ? static_cast<double>(samples) /
                                static_cast<double>(sampleRate)
                          : 0.0;
}

constexpr SampleTick secondsToSamples(double seconds, int sampleRate) noexcept {
    return sampleRate > 0
               ? static_cast<SampleTick>(seconds * static_cast<double>(sampleRate))
               : 0;
}

constexpr double samplesToMs(SampleTick samples, int sampleRate) noexcept {
    return samplesToSeconds(samples, sampleRate) * 1000.0;
}

// 人类可读的采样区间，如 "[1024,2048)"
std::string toString(SampleRange range);

} // namespace mediacore
