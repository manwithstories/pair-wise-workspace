// 批处理命令入口：
//   media_batch demo --seed N --bandwidth-kbps B --target-kbps T \
//       --checkpoint-packets N --output DIR
// 退出码：0 完成且指标达标；2 参数/输入错误；3 有不可恢复区间；4 检查点或输出写失败。
#include "media/demo_generator.hpp"
#include "media/error_model.hpp"
#include "media/pipeline.hpp"

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <string>

namespace {

constexpr double kMaxBitrateDeltaPct = 8.0;
constexpr double kMinPsnrDb = 30.0;
constexpr double kMaxAlignErrorMs = 20.0;

void usage() {
    std::fprintf(stderr,
                 "usage: media_batch demo [--seed N] [--bandwidth-kbps N] "
                 "[--target-kbps N] [--checkpoint-packets N] --output DIR\n");
}

// 拒绝输出目录内的路径穿越：任何 ".." 组件都不允许。
bool path_is_safe(const std::string& p) {
    for (const auto& comp : std::filesystem::path(p))
        if (comp == "..") return false;
    return !p.empty();
}

} // namespace

int main(int argc, char** argv) {
    if (argc < 2 || std::strcmp(argv[1], "demo") != 0) {
        usage();
        return 2;
    }
    media::DemoConfig cfg;
    bool have_output = false;
    for (int i = 2; i < argc; ++i) {
        auto need = [&](const char* name) -> const char* {
            if (i + 1 >= argc) {
                std::fprintf(stderr, "missing value for %s\n", name);
                std::exit(2);
            }
            return argv[++i];
        };
        if (std::strcmp(argv[i], "--seed") == 0)
            cfg.seed = std::strtoull(need("--seed"), nullptr, 10);
        else if (std::strcmp(argv[i], "--bandwidth-kbps") == 0)
            cfg.bandwidth_kbps = std::strtoull(need("--bandwidth-kbps"), nullptr, 10);
        else if (std::strcmp(argv[i], "--target-kbps") == 0)
            cfg.target_kbps = std::strtoull(need("--target-kbps"), nullptr, 10);
        else if (std::strcmp(argv[i], "--checkpoint-packets") == 0)
            cfg.checkpoint_packets =
                static_cast<uint32_t>(std::strtoul(need("--checkpoint-packets"), nullptr, 10));
        else if (std::strcmp(argv[i], "--output") == 0) {
            cfg.output_dir = need("--output");
            have_output = true;
        } else {
            std::fprintf(stderr, "unknown argument: %s\n", argv[i]);
            usage();
            return 2;
        }
    }
    if (!have_output || !path_is_safe(cfg.output_dir) ||
        cfg.target_kbps == 0 || cfg.checkpoint_packets == 0) {
        usage();
        return 2;
    }

    media::ErrorModel errors;
    media::DemoTrace trace = media::generate_demo_trace(cfg);
    if (trace.total_data_packets == 0) {
        std::fprintf(stderr, "empty input trace\n");
        return 2;
    }

    // 第一次运行：模拟批处理中断（处理 60% 后停止），已提交块保留。
    media::run_pipeline(cfg, trace, errors, trace.arrivals.size() * 3 / 5);
    // 第二次运行：从检查点续跑，跳过已提交块，直至完成。
    media::PipelineResult res =
        media::run_pipeline(cfg, trace, errors, UINT64_MAX);

    const media::ManifestData& m = res.manifest;
    std::printf("target_kbps=%llu\n", (unsigned long long)m.target_kbps);
    std::printf("actual_kbps=%llu\n", (unsigned long long)m.actual_kbps);
    std::printf("bitrate_delta_pct=%.2f\n", m.bitrate_delta_pct);
    std::printf("quality_psnr_db=%.2f\n", m.quality_psnr_db);
    std::printf("recovered_packets=%llu\n", (unsigned long long)m.recovered_packets);
    std::printf("duplicate_packets=%llu\n", (unsigned long long)m.duplicate_packets);
    std::printf("reordered_packets=%llu\n", (unsigned long long)m.reordered_packets);
    std::printf("manifest_max_error_ms=%.2f\n", m.manifest_max_error_ms);
    std::printf("checkpoint_resume=%s\n", res.resumed ? "true" : "false");

    if (res.write_failed) return 4;
    if (res.unrecoverable > 0) return 3;
    bool metrics_ok = std::abs(m.bitrate_delta_pct) <= kMaxBitrateDeltaPct &&
                      m.quality_psnr_db >= kMinPsnrDb &&
                      m.manifest_max_error_ms <= kMaxAlignErrorMs;
    return metrics_ok ? 0 : 1;
}
