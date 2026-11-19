// silence.hpp — libswresample 归一化音频 + 窗口静音检测（损坏帧不计静音）
#pragma once
#include "common.hpp"
#include <vector>
#include <memory>

namespace md {

struct SilenceCounters {
    uint32_t decoded_frames = 0;
    uint32_t corrupt_frames = 0;
    uint32_t silence_windows = 0;
};

// 有界 PCM 缓存：固定上限，峰值内存不随输入时长增长
class SilenceDetector {
public:
    explicit SilenceDetector(const ProcessConfig& cfg);
    ~SilenceDetector();

    // 喂入一个音频 RTP 包（PCM s16le 或 AAC ADTS/raw）
    void push(const MediaPacket& p, std::vector<PipelineError>& errors);

    // 冲刷半窗残留并合并重叠静音边界
    std::vector<SilenceInterval> finish();

    const std::vector<SilenceInterval>& intervals() const { return intervals_; }
    const SilenceCounters& counters() const { return counters_; }
    // 规范化采样摘要（指纹碰撞复核使用）：每包能量摘要
    const std::vector<uint32_t>& sample_digest() const { return digest_; }

private:
    void decode_aac(const uint8_t* data, size_t len,
                    std::vector<int16_t>& out, std::vector<PipelineError>& errors,
                    uint32_t seq);
    void analyze_pcm(const int16_t* samples, size_t n, int64_t begin_us,
                     uint32_t sample_rate, uint32_t channels);

    const ProcessConfig& cfg_;
    struct Impl;
    std::unique_ptr<Impl> impl_;
    std::vector<SilenceInterval> intervals_;
    std::vector<uint32_t> digest_;
    SilenceCounters counters_;
    // 有界环形 PCM 缓存（s16 单声道 16k）
    std::vector<int16_t> ring_;
    size_t ring_cap_ = 0, ring_head_ = 0, ring_size_ = 0;
    int64_t ring_time_head_us_ = 0;
    bool ring_time_valid_ = false;
};

} // namespace md
