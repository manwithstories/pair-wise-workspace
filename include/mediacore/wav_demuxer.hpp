// mediacore/wav_demuxer.hpp — 增量 WAV/PCM 解复用器
//
// 设计要点：
//  * 逐块读取而非整文件载入，适配慢速存储；块大小固定为 blockFrames 帧。
//  * 内置哨兵标记用于确定性的恢复测试（由夹具生成器写入）：
//      "MCOR"  中段损坏（8 字节：标记 + 损坏字节长度）
//      "MGAP"  时间戳缺口（8 字节：标记 + 缺口帧数）
//      "MDUP"  块重复（8 字节：标记 + 重复帧数，随后为重复 PCM）
//  * RIFF 块目录缺失/不完整时按 scanLimitBytes 顺序扫描重建索引。
//  * 尾部不足一个完整帧时截断并以 IncompleteTail 报告。
#pragma once

#include <cstdint>
#include <fstream>
#include <string>
#include <vector>

#include "mediacore/error.hpp"
#include "mediacore/pcm.hpp"
#include "mediacore/time.hpp"

namespace mediacore {

struct DemuxConfig {
    SampleTick blockFrames    = 256;   // 每块帧数（采样刻度）
    std::int64_t scanLimitBytes = 1 << 20; // 顺序扫描重建索引上限
};

struct DemuxStats {
    std::uint64_t blocksEmitted   = 0;
    std::uint64_t bytesScanned    = 0;  // 顺序扫描消耗字节
    std::uint64_t tailBytesDropped = 0; // 尾部不完整样本字节
    bool indexRebuilt = false;
    std::vector<MediaError> recoverableErrors;
};

class WavDemuxer {
public:
    WavDemuxer() = default;
    ~WavDemuxer();

    WavDemuxer(const WavDemuxer&) = delete;
    WavDemuxer& operator=(const WavDemuxer&) = delete;

    // 打开并解析头部；不可恢复错误（截断头部/格式不支持/扫描超限）经 Result 返回
    Result<void> open(const std::string& path, const DemuxConfig& config);
    void close();

    // 取下一块；文件正常结束返回 false 且 out 不被写入。
    // 可恢复情况（损坏/缺口/重复/尾部截断）通过块分类与 stats 报告。
    bool nextBlock(AudioBlock& out, MediaError& err);

    const PcmFormat& format() const noexcept { return format_; }
    const DemuxStats& stats() const noexcept { return stats_; }
    SampleTick totalFrames() const noexcept { return totalFrames_; }
    bool eof() const noexcept { return eof_; }

private:
    bool parseHeader(MediaError& err);
    bool locateDataChunk(MediaError& err);
    bool readExact(char* dst, std::size_t n, MediaError& err);
    bool readSentinel(AudioBlock& out, bool& consumed, MediaError& err);

    std::ifstream file_;
    std::string path_;
    DemuxConfig config_;
    PcmFormat format_;
    DemuxStats stats_;

    ByteOffset dataStart_   = 0; // data 块载荷起点
    ByteOffset dataLength_  = 0; // data 块声明长度（-1 表示未知，读到 EOF）
    std::uint32_t riffSize_ = 0;
    ByteOffset cursor_      = 0; // data 区内已消费字节
    SampleTick nextSample_  = 0; // 下一个输出块期望的全局采样刻度
    SampleTick totalFrames_ = 0;
    bool eof_ = false;
};

} // namespace mediacore
