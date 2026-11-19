#pragma once
// 跨模块共用的错误分类、损坏区间、恢复记录与汇总结果。
#include "media_clock.hpp"

#include <cstdint>
#include <optional>
#include <string>
#include <vector>

namespace ma {

enum class ErrorCategory {
    None,
    DecodeFailed,   // 解码失败 / 码流损坏包
    Truncated,      // 文件截断 / IO 提前结束
    CorruptPacket,  // 容器层损坏包
    Timestamp,      // 时间戳异常
};

const char* toString(ErrorCategory c);

// 单个时间戳异常（原始值保留供复核）。
struct TsAnomaly {
    int streamIndex = 0;
    TsKind kind = TsKind::Normal;
    int64_t rawTs = 0;
    int64_t expandedTs = 0;
    int64_t packetPos = -1;
};

// 损坏区间：起止包位置与边界时间（ms），含恢复统计。
struct DamageRange {
    ErrorCategory category = ErrorCategory::None;
    int64_t startPacket = -1;
    int64_t endPacket = -1;
    int64_t startFilePos = -1;
    int64_t endFilePos = -1;
    double startMs = 0.0;
    double endMs = 0.0;
    int errorPackets = 0;       // 区间内错误包数
    int recoveredPackets = 0;   // 区间后恢复成功的包数
    std::string detail;
};

// 解码恢复记录（错误模型第 4 条能力的量化基础）。
struct RecoveryRecord {
    int64_t totalPackets = 0;     // 预期包数
    int64_t errorPackets = 0;     // 损坏/失败包数
    int64_t recoveredPackets = 0; // 损坏之后仍可解码的包数
    double recoveryRate() const {
        if (totalPackets <= 0) return 0.0;
        return 100.0 * static_cast<double>(recoveredPackets) /
               static_cast<double>(totalPackets);
    }
};

// 单流摘要：容器/编码参数/时间基/包数。
struct StreamSummary {
    int index = 0;
    bool isVideo = false;
    std::string codec;
    std::string profile;
    int width = 0;
    int height = 0;
    int timeBaseNum = 0;
    int timeBaseDen = 1;
    int64_t packetCount = 0;
    int64_t frameCount = 0;
    int64_t keyframeCount = 0;
    double durationMs = 0.0;
    double bitrateKbps = 0.0;
    // VFR 帧间隔分布（毫秒）。
    double frameIntervalMinMs = 0.0;
    double frameIntervalMaxMs = 0.0;
    double frameIntervalMeanMs = 0.0;
    double frameIntervalP95Ms = 0.0;
};

// 交付阈值（命令行可覆盖）。
struct Thresholds {
    double avSyncErrorMs = 20.0;
    double recoveryRatePct = 90.0;
    double pipelineLatencyP95Ms = 2000.0;
    double peakMemoryMiB = 64.0;
};

// 单文件分析结果（JSON 输出字段与此一一对应）。
struct AuditResult {
    std::string file;
    bool ok = false;                 // 分析流程本身是否完成
    std::string fatalError;          // 内部/输入错误说明
    std::string containerFormat;
    double durationMs = 0.0;
    int64_t fileSizeBytes = 0;

    std::vector<StreamSummary> streams;
    std::vector<DamageRange> damageRanges;
    std::vector<TsAnomaly> tsAnomalies;
    RecoveryRecord recovery;

    // GOP 统计（视频流）。
    double gopIntervalMeanMs = 0.0;
    double gopIntervalP95Ms = 0.0;
    int gopAnomalies = 0;

    // 音画同步（无音频时为空，阈值检查跳过该项）。
    bool hasAudio = false;
    double avSyncErrorBeforeMs = 0.0;
    double avSyncErrorAfterMs = 0.0;
    double driftPpm = 0.0;

    // 管线运行指标。
    double pipelineLatencyP95Ms = 0.0;
    double peakMemoryMiB = 0.0;
    int backpressureEvents = 0;
    bool cancelled = false;
    bool timeout = false;

    // 阈值判定：返回未达标项的人类可读说明（空向量=全部达标）。
    std::vector<std::string> evaluate(const Thresholds& t) const {
        std::vector<std::string> fails;
        if (hasAudio && avSyncErrorAfterMs > t.avSyncErrorMs)
            fails.push_back("av_sync_error_ms 超阈值");
        if (recovery.totalPackets > 0 &&
            recovery.recoveryRate() < t.recoveryRatePct)
            fails.push_back("recovery_rate_pct 低于阈值");
        if (pipelineLatencyP95Ms > t.pipelineLatencyP95Ms)
            fails.push_back("pipeline_latency_p95_ms 超阈值");
        if (peakMemoryMiB > t.peakMemoryMiB)
            fails.push_back("peak_memory_mib 超上限");
        return fails;
    }
};

} // namespace ma
