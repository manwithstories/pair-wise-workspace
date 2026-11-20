// mediacore/fixture.hpp — 测试夹具生成器
//
// 全部样例数据由 C++ 代码确定性生成（正弦扫幅 + 满幅样本），
// 不引用任何外部文件。支持注入哨兵（损坏/缺口/重复）、截断尾部、
// 破坏 RIFF 索引以及裸 PCM 写入。
#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "mediacore/pcm.hpp"

namespace mediacore::fixture {

// 确定性采样：扫幅正弦，每 256 帧出现一个满幅样本（保证 peak_abs=32767）
std::int16_t sampleAt(SampleTick frame, int channel, int channels);

// 生成 nFrames 帧的交错 s16 PCM
std::vector<std::uint8_t> makePcm(const PcmFormat& format, SampleTick nFrames);

struct WavBuilder {
    PcmFormat format{16000, 1, 16, SampleFormat::S16Le};
    std::vector<std::uint8_t> bytes;

    void writeU32(std::uint32_t v);
    void writeU16(std::uint16_t v);
    void writeBytes(const std::vector<std::uint8_t>& data);
    void writeRaw(const void* p, std::size_t n);

    // 写完整 RIFF/WAVE 头（dataSize 可先给 0，由 finish 回填）
    void writeHeader(std::uint32_t dataSize);
    // 注入哨兵
    void writeCorruptSentinel(std::uint32_t corruptBytes);
    void writeGapSentinel(std::uint32_t gapFrames);
    void writeDuplicateSentinel(std::uint32_t dupFrames,
                                const std::vector<std::uint8_t>& dupPcm);
    // 回填 RIFF/data 长度
    void finish();
};

// 直接落盘的便捷封装
bool writeFile(const std::string& path, const std::vector<std::uint8_t>& bytes);

// 生成验收用 4 个夹具（见 main.cpp selftest），返回实际写入的文件路径
struct FixtureSet {
    std::string cleanWav;        // 完全正常
    std::string truncatedTail;   // 尾部多 3 字节，不完整样本
    std::string midCorrupt;      // 中段损坏 + 缺口 + 重复（可恢复）
    std::string badHeader;       // RIFF 头被截断（不可恢复）
};
FixtureSet makeAcceptanceFixtures(const std::string& dir);

} // namespace mediacore::fixture
