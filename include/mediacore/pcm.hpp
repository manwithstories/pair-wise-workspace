// mediacore/pcm.hpp — PCM 采样格式与流音频块
//
// 采集端交付的原始采样块统一抽象为 AudioBlock：交错 (interleaved) 的
// 有符号 16bit PCM，附带该块在统一采样刻度上的起始时间戳与分类。
// 块大小由 fmt 块对齐决定；非整块尾部以 IncompleteTail 分类并截断。
#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "mediacore/error.hpp"
#include "mediacore/time.hpp"

namespace mediacore {

// 目前仅支持有符号 16bit little-endian（采集端 PCM/WAV 主格式）。
enum class SampleFormat {
    S16Le,
};

struct PcmFormat {
    int sampleRate = 0;   // Hz，如 48000 / 16000
    int channels   = 0;   // 交错声道数
    int bitsPerSample = 0;
    SampleFormat sampleFormat = SampleFormat::S16Le;

    // 每帧（一个采样时刻的全部声道）字节数
    int frameBytes() const noexcept {
        return channels * (bitsPerSample / 8);
    }
    // WAV 块对齐 = frameBytes
    int blockAlign() const noexcept { return frameBytes(); }
    // 字节缓冲区可容纳的完整帧数
    SampleTick framesInBytes(ByteOffset bytes) const noexcept {
        return frameBytes() > 0 ? bytes / frameBytes() : 0;
    }
    bool valid() const noexcept {
        return sampleRate > 0 && channels > 0 && bitsPerSample == 16 &&
               frameBytes() > 0;
    }
};

// 音频块分类，决定下游保留 / 跳过 / 截断行为
enum class BlockKind {
    Normal,          // 正常 PCM 块
    IncompleteTail,  // 尾部不足一个完整帧，样本被截断到最后一个完整帧
    Corrupt,         // 中段损坏区间
    Gap,             // 时间戳缺口（无样本，仅占位记录跳过范围）
    Duplicate,       // 重复块，样本与此前区间重叠
};

const char* blockKindName(BlockKind kind) noexcept;

struct AudioBlock {
    PcmFormat format;
    SampleTick startSample = 0;                 // 该块首帧的全局采样刻度
    std::vector<std::uint8_t> interleavedPcm;   // 交错 s16le
    BlockKind kind = BlockKind::Normal;

    SampleTick frameCount() const noexcept {
        return format.framesInBytes(
            static_cast<ByteOffset>(interleavedPcm.size()));
    }
    SampleTick endSample() const noexcept {
        return startSample + frameCount();
    }
};

// 将字节缓冲截断到最后一个完整帧（丢弃半样本），返回丢弃字节数
std::size_t truncateToFrameBoundary(std::vector<std::uint8_t>& buffer,
                                    const PcmFormat& format) noexcept;

} // namespace mediacore
