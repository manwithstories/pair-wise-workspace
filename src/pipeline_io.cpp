#include "pipeline_io.hpp"
#include "pipeline.hpp"
#include "sha256.hpp"
#include "util.hpp"
#include <filesystem>
#include <fstream>

namespace md {
namespace fs = std::filesystem;

std::string segment_dir(const ProcessConfig& cfg) {
    if (!cfg.work_dir.empty()) return cfg.work_dir;
    fs::path out(cfg.out_path);
    fs::path base = out.has_parent_path() ? out.parent_path() : fs::path(".");
    return (base / "delivery_segments").string();
}

std::string segment_blob_path(const ProcessConfig& cfg, uint32_t id) {
    char buf[32];
    std::snprintf(buf, sizeof(buf), "/segment_%06u.bin", id);
    return segment_dir(cfg) + buf;
}

bool write_segment_blob(const ProcessConfig& cfg, const PipelineHooks& hooks,
                        uint32_t id, const std::vector<uint8_t>& bytes,
                        PipelineError& err) {
    // 测试钩子：模拟磁盘写失败，携带阶段与片段/序号
    if (!hooks.io_fail_path.empty() &&
        segment_blob_path(cfg, id).find(hooks.io_fail_path) != std::string::npos) {
        err = {ErrType::DiskWriteFailed, stage::Segment, id,
               "injected disk write failure"};
        return false;
    }
    std::string io_err;
    if (!atomic_write(segment_blob_path(cfg, id), bytes, io_err)) {
        err = {ErrType::DiskWriteFailed, stage::Segment, id, io_err};
        return false;
    }
    return true;
}

bool verify_segment_blob(const ProcessConfig& cfg, const SegmentCheckpoint& sc,
                         std::vector<PipelineError>& errors) {
    std::ifstream f(segment_blob_path(cfg, sc.id), std::ios::binary);
    if (!f) {
        errors.push_back({ErrType::SegmentHashMismatch, stage::Checkpoint,
                          sc.id, "segment blob missing on resume"});
        return false;
    }
    Sha256 h;
    std::vector<uint8_t> buf(65536);
    while (f) {
        f.read(reinterpret_cast<char*>(buf.data()),
               static_cast<std::streamsize>(buf.size()));
        auto n = f.gcount();
        if (n > 0) h.update(buf.data(), size_t(n));
    }
    if (Sha256::hex(h.finish()) != sc.sha256_hex) {
        errors.push_back({ErrType::SegmentHashMismatch, stage::Checkpoint,
                          sc.id, "segment hash mismatch on resume"});
        return false;
    }
    return true;
}

ExitCode checkpoint_compat(const ProcessConfig& cfg,
                           const std::array<uint8_t, 32>& input_sha,
                           const std::array<uint8_t, 32>& params_sha,
                           CheckpointData& cp, bool& had_checkpoint) {
    had_checkpoint = false;
    if (!cfg.resume) return ExitCode::Ok;
    PipelineError err;
    auto loaded = load_checkpoint(cfg.checkpoint_path, err);
    if (!loaded) {
        if (err.type == ErrType::CheckpointMismatch) {
            cp = CheckpointData{};
            return ExitCode::IncompatibleCheckpoint; // 损坏的检查点
        }
        return ExitCode::Ok;                        // 无检查点，首次运行
    }
    CpReject rej = validate_checkpoint(*loaded, input_sha, params_sha);
    if (rej != CpReject::None) {
        cp = *loaded;
        return ExitCode::IncompatibleCheckpoint;
    }
    cp = *loaded;
    had_checkpoint = true;
    return ExitCode::Ok;
}

} // namespace md
