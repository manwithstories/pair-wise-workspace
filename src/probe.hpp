#pragma once
// 容器、轨道与码流结构探测（只读打开，不修改输入）。
#include "error_model.hpp"
#include "media_types.hpp"

#include <string>
#include <vector>

extern "C" {
#include <libavformat/avformat.h>
}

namespace ma {

// 单条流的探测期元数据（供管线各阶段共享）。
struct ProbeStream {
    int index = 0;
    bool isVideo = false;
    AVCodecParameters* codecpar = nullptr;
    const AVCodec* decoder = nullptr;
    AVRational timeBase{};
    std::string codecName;
    std::string profileName;
};

struct ProbeInfo {
    AVFormatContext* fmt = nullptr;
    std::string formatName;
    std::string formatLongName;
    int64_t fileSize = 0;
    double durationMs = 0.0;
    std::vector<ProbeStream> streams;
    int videoIndex = -1;
    int audioIndex = -1;

    ~ProbeInfo();
};

// 打开并探测文件；失败时返回 false 并填充 err（退出码 2 场景）。
bool probeOpen(const std::string& path, ProbeInfo& out, std::string& err);

// 由探测信息生成可读流摘要（功能 1 的输出基础）。
StreamSummary summarizeStream(const ProbeStream& ps, double durationMs);

} // namespace ma
