#include "media/manifest.hpp"

#include <cmath>
#include <fstream>

namespace media {

double bitrate_delta_pct(uint64_t target_kbps, uint64_t actual_kbps) {
    if (target_kbps == 0) return 0.0;
    return (static_cast<double>(actual_kbps) - static_cast<double>(target_kbps)) *
           100.0 / static_cast<double>(target_kbps);
}

double estimate_psnr(double bitrate_ratio, double unrecoverable_ratio) {
    double psnr = 46.0;
    if (bitrate_ratio < 1.0)
        psnr -= 18.0 * (1.0 - bitrate_ratio); // 码率不足惩罚
    psnr -= 25.0 * unrecoverable_ratio;       // 不可恢复区间惩罚
    return psnr;
}

bool write_manifest(const std::filesystem::path& path, const ManifestData& d,
                    ErrorModel& errors) {
    std::ofstream out(path, std::ios::trunc);
    if (!out) {
        errors.record({ErrorKind::WriteFailure, "manifest", -1, -1, -1,
                       "cannot open manifest for write: " + path.string()});
        return false;
    }
    auto j = [](const char* key, double v, bool comma = true) {
        std::string s = "  \"";
        s += key;
        s += "\": ";
        char buf[64];
        std::snprintf(buf, sizeof(buf), "%.4f", v);
        s += buf;
        if (comma) s += ',';
        return s + '\n';
    };
    out << "{\n";
    out << j("target_kbps", static_cast<double>(d.target_kbps));
    out << j("actual_kbps", static_cast<double>(d.actual_kbps));
    out << j("bitrate_delta_pct", d.bitrate_delta_pct);
    out << j("quality_psnr_db", d.quality_psnr_db);
    out << j("recovered_packets", static_cast<double>(d.recovered_packets));
    out << j("duplicate_packets", static_cast<double>(d.duplicate_packets));
    out << j("reordered_packets", static_cast<double>(d.reordered_packets));
    out << j("lost_packets", static_cast<double>(d.lost_packets));
    out << j("unrecoverable_packets",
             static_cast<double>(d.unrecoverable_packets));
    out << j("recovery_rate", d.recovery_rate);
    out << j("av_offset_ms", d.av_offset_ms);
    out << j("av_drift_ppm", d.av_drift_ppm);
    out << j("av_residual_ms", d.av_residual_ms);
    out << j("manifest_max_error_ms", d.manifest_max_error_ms);
    out << j("subtitle_cues", static_cast<double>(d.subtitle_cues));
    out << j("subtitle_bad_records",
             static_cast<double>(d.subtitle_bad_records));
    out << j("gaps", static_cast<double>(d.gaps));
    out << j("chunks_committed", static_cast<double>(d.chunks_committed));
    out << "  \"checkpoint_resume\": " << (d.checkpoint_resume ? "true" : "false")
        << '\n';
    out << "}\n";
    out.flush();
    if (!out) {
        errors.record({ErrorKind::WriteFailure, "manifest", -1, -1, -1,
                       "manifest write failed"});
        return false;
    }
    return true;
}

} // namespace media
