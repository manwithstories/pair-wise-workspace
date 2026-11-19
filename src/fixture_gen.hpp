#pragma once
// 确定性自测素材生成与故障注入；全部本地生成，不引用外部素材、不访问网络。
#include <cstdint>
#include <string>

namespace ma {

struct FixtureOptions {
    std::string path;
    int width = 320;
    int height = 240;
    int durationSec = 6;
    int videoFps = 25;          // 名义帧率
    int sampleRate = 48000;
    int gopFrames = 25;         // 固定 GOP，便于损坏恢复与 GOP 统计验证
    bool vfr = false;           // 可变帧间隔模式
    double audioDriftPpm = 0.0; // 音频渐进漂移
    bool faststart = false;     // moov 前置（截断夹具需要）
    uint64_t seed = 42;
};

// 生成基础 MP4（H.264 + AAC），确定性内容。
bool generateFixture(const FixtureOptions& o, std::string& err);

// 二进制后处理：在 ctts 盒注入 2^31 级 PTS 回绕（指定帧序号）。
bool injectTimestampWrap(const std::string& path, int frameIndex,
                         std::string& err);

// 二进制后处理：在 mdat 内确定性损坏一个视频 NAL 单元。
// 返回损坏处字节偏移；损坏包数量写入 corruptPackets。
bool injectCorruptPacket(const std::string& path, uint64_t seed,
                         int64_t& posOut, int& corruptPackets,
                         std::string& err);

// 二进制后处理：保留前 keepRatio 字节（faststart 文件截断后仍可探测）。
bool truncateFile(const std::string& path, double keepRatio,
                  int64_t& newSize, std::string& err);

} // namespace ma
