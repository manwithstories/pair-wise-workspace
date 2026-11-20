// mediacore/manifest.hpp — 输出清单（时间线 + 统计 + 恢复记录）
//
// 清单与质量统计共享同一个有效 SampleRange，确保 manifest 声明的
// 时间线与质量指标覆盖完全一致的样本集合。仅在所有输出成功提交后
// 才允许落盘（写入失败返回错误，不生成看似成功的清单）。
#pragma once

#include <string>
#include <vector>

#include "mediacore/error.hpp"
#include "mediacore/quality.hpp"
#include "mediacore/time.hpp"
#include "mediacore/track_mapper.hpp"

namespace mediacore {

struct SkippedSpan {
    SampleRange range;
    std::string reason;   // 错误码短名
    std::string detail;
};

struct TrackManifest {
    std::string name;
    std::string layout;
    std::string outputFile;
    SampleRange validRange{};
    SampleTick alignmentErrorSamples = 0; // 首帧未对齐预期的样本数
};

struct Manifest {
    std::string sourceFile;
    int sampleRate = 0;
    SampleRange validRange{};                 // 与质量统计同源
    std::vector<SkippedSpan> skipped;
    std::vector<TrackManifest> tracks;
    QualityStats quality;
    std::vector<std::string> recoverableCodes;
    bool recovered = false;

    // 序列化为稳定、可脚本解析的 JSON（键顺序固定）
    std::string toJson() const;
};

// 原子写清单：先写 .part 再改名
Result<void> writeManifest(const std::string& path, const Manifest& m);

} // namespace mediacore
