#include "report.hpp"

#include <filesystem>
#include <fstream>
#include <cstdio>
#include <sstream>

namespace ma {
namespace {

std::string esc(const std::string& s) {
    std::string o;
    o.reserve(s.size() + 2);
    for (char c : s) {
        switch (c) {
        case '"':  o += "\\\""; break;
        case '\\': o += "\\\\"; break;
        case '\n': o += "\\n";  break;
        case '\r': o += "\\r";  break;
        case '\t': o += "\\t";  break;
        default:
            if (static_cast<unsigned char>(c) < 0x20) {
                char buf[8];
                std::snprintf(buf, sizeof(buf), "\\u%04x", c);
                o += buf;
            } else o += c;
        }
    }
    return o;
}

std::string fnum(double v) {
    std::ostringstream os;
    os.precision(3);
    os << std::fixed << v;
    return os.str();
}

} // namespace

std::string toJson(const AuditResult& r) {
    std::ostringstream o;
    o << "{";
    o << "\"file\":\"" << esc(r.file) << "\"";
    o << ",\"ok\":" << (r.ok ? "true" : "false");
    if (!r.fatalError.empty())
        o << ",\"fatal_error\":\"" << esc(r.fatalError) << "\"";
    o << ",\"container\":\"" << esc(r.containerFormat) << "\"";
    o << ",\"duration_ms\":" << fnum(r.durationMs);
    o << ",\"file_size_bytes\":" << r.fileSizeBytes;

    o << ",\"streams\":[";
    for (size_t i = 0; i < r.streams.size(); ++i) {
        const auto& s = r.streams[i];
        if (i) o << ",";
        o << "{\"index\":" << s.index
          << ",\"type\":\"" << (s.isVideo ? "video" : "audio") << "\""
          << ",\"codec\":\"" << esc(s.codec) << "\""
          << ",\"profile\":\"" << esc(s.profile) << "\""
          << ",\"time_base\":\"" << s.timeBaseNum << "/" << s.timeBaseDen << "\""
          << ",\"packets\":" << s.packetCount
          << ",\"frames\":" << s.frameCount
          << ",\"keyframes\":" << s.keyframeCount
          << ",\"bitrate_kbps\":" << fnum(s.bitrateKbps)
          << ",\"frame_interval_min_ms\":" << fnum(s.frameIntervalMinMs)
          << ",\"frame_interval_max_ms\":" << fnum(s.frameIntervalMaxMs)
          << ",\"frame_interval_mean_ms\":" << fnum(s.frameIntervalMeanMs)
          << "}";
    }
    o << "]";

    o << ",\"damage_ranges\":[";
    for (size_t i = 0; i < r.damageRanges.size(); ++i) {
        const auto& d = r.damageRanges[i];
        if (i) o << ",";
        o << "{\"category\":\"" << toString(d.category) << "\""
          << ",\"start_packet\":" << d.startPacket
          << ",\"end_packet\":" << d.endPacket
          << ",\"start_file_pos\":" << d.startFilePos
          << ",\"end_file_pos\":" << d.endFilePos
          << ",\"start_ms\":" << fnum(d.startMs)
          << ",\"end_ms\":" << fnum(d.endMs)
          << ",\"error_packets\":" << d.errorPackets
          << ",\"recovered_packets\":" << d.recoveredPackets
          << ",\"detail\":\"" << esc(d.detail) << "\"}";
    }
    o << "]";

    o << ",\"timestamp_anomalies\":" << r.tsAnomalies.size();
    o << ",\"recovery_rate_pct\":" << fnum(r.recovery.recoveryRate());
    o << ",\"total_packets\":" << r.recovery.totalPackets;
    o << ",\"error_packets\":" << r.recovery.errorPackets;
    o << ",\"recovered_packets\":" << r.recovery.recoveredPackets;
    o << ",\"gop_interval_p95_ms\":" << fnum(r.gopIntervalP95Ms);
    o << ",\"gop_anomalies\":" << r.gopAnomalies;
    o << ",\"has_audio\":" << (r.hasAudio ? "true" : "false");
    if (r.hasAudio) {
        o << ",\"av_sync_error_ms\":" << fnum(r.avSyncErrorAfterMs);
        o << ",\"av_sync_error_before_ms\":" << fnum(r.avSyncErrorBeforeMs);
        o << ",\"av_drift_ppm\":" << fnum(r.driftPpm);
    } else {
        o << ",\"av_sync_error_ms\":null";
    }
    o << ",\"pipeline_latency_p95_ms\":" << fnum(r.pipelineLatencyP95Ms);
    o << ",\"peak_memory_mib\":" << fnum(r.peakMemoryMiB);
    o << ",\"backpressure_events\":" << r.backpressureEvents;
    o << ",\"cancelled\":" << (r.cancelled ? "true" : "false");
    o << ",\"timeout\":" << (r.timeout ? "true" : "false");
    o << "}";
    return o.str();
}

bool appendJsonLine(const std::string& jsonlPath,
                    const std::string& inputPath,
                    const std::string& line, std::string& err) {
    std::error_code ec;
    if (!inputPath.empty() &&
        std::filesystem::exists(jsonlPath, ec) &&
        std::filesystem::equivalent(jsonlPath, inputPath, ec)) {
        err = "拒绝写入：输出路径与输入文件相同";
        return false;
    }
    std::ofstream f(jsonlPath, std::ios::binary | std::ios::app);
    if (!f) { err = "无法打开 JSONL 输出: " + jsonlPath; return false; }
    f << line << "\n";
    return bool(f);
}

std::string toTerminalSummary(const AuditResult& r,
                              const Thresholds& th) {
    std::ostringstream o;
    o << "── " << r.file << " ────────────────────────────\n";
    if (!r.ok) {
        o << "  分析失败: " << r.fatalError << "\n";
        return o.str();
    }
    o << "  容器: " << r.containerFormat
      << "  时长: " << fnum(r.durationMs) << " ms"
      << "  流数: " << r.streams.size() << "\n";
    for (const auto& s : r.streams)
        o << "  [" << s.index << "] " << (s.isVideo ? "视频 " : "音频 ")
          << s.codec << " " << s.profile
          << (s.isVideo ? " " + std::to_string(s.width) + "x" +
                              std::to_string(s.height) : "")
          << " 包数=" << s.packetCount
          << " 时间基=" << s.timeBaseNum << "/" << s.timeBaseDen << "\n";
    o << "  损坏区间: " << r.damageRanges.size()
      << "  时间戳异常: " << r.tsAnomalies.size() << "\n";
    o << "  recovery_rate_pct=" << fnum(r.recovery.recoveryRate())
      << "  gop_p95_ms=" << fnum(r.gopIntervalP95Ms);
    if (r.hasAudio)
        o << "  av_sync_error_ms=" << fnum(r.avSyncErrorAfterMs)
          << " (校正前 " << fnum(r.avSyncErrorBeforeMs) << ")"
          << "  drift_ppm=" << fnum(r.driftPpm);
    o << "\n";
    o << "  latency_p95_ms=" << fnum(r.pipelineLatencyP95Ms)
      << "  peak_memory_mib=" << fnum(r.peakMemoryMiB)
      << "  背压=" << r.backpressureEvents << "\n";
    const auto fails = r.evaluate(th);
    o << "  阈值: " << (fails.empty() ? "全部达标 ✓" : "未达标 ✗");
    for (const auto& f : fails) o << "\n    - " << f;
    o << "\n";
    return o.str();
}

std::string toBatchSummary(size_t total, size_t failed,
                           size_t thresholdMissed) {
    std::ostringstream o;
    o << "批处理完成: 共 " << total << " 个文件，失败 " << failed
      << " 个，指标未达标 " << thresholdMissed << " 个，成功 "
      << (total - failed - thresholdMissed) << " 个\n";
    return o.str();
}

} // namespace ma
