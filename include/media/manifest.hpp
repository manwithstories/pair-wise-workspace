// 对齐清单：汇总码率、质量、音画偏移与边界误差，落盘为 JSON。
#pragma once

#include "media/error_model.hpp"
#include "media/recovery.hpp"

#include <cstdint>
#include <filesystem>
#include <string>

namespace media {

struct ManifestData {
    uint64_t target_kbps = 0;
    uint64_t actual_kbps = 0;
    double bitrate_delta_pct = 0.0;
    double quality_psnr_db = 0.0;

    uint64_t recovered_packets = 0;
    uint64_t duplicate_packets = 0;
    uint64_t reordered_packets = 0;
    uint64_t lost_packets = 0;
    uint64_t unrecoverable_packets = 0;
    double recovery_rate = 1.0;

    double av_offset_ms = 0.0;
    double av_drift_ppm = 0.0;
    double av_residual_ms = 0.0;
    double manifest_max_error_ms = 0.0; // 清单边界误差（对齐误差上限）

    uint64_t subtitle_cues = 0;
    uint64_t subtitle_bad_records = 0;
    uint64_t gaps = 0;
    uint32_t chunks_committed = 0;
    bool checkpoint_resume = false;
};

// 计算码率偏差百分比（目标为 0 时返回 0）。
double bitrate_delta_pct(uint64_t target_kbps, uint64_t actual_kbps);

// 质量模型：基准 46 dB，按码率缺口与不可恢复占比惩罚。
double estimate_psnr(double bitrate_ratio, double unrecoverable_ratio);

// 写清单；失败记录 WriteFailure 并返回 false。
bool write_manifest(const std::filesystem::path& path, const ManifestData& data,
                    ErrorModel& errors);

} // namespace media
