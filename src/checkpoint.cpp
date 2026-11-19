#include "checkpoint.hpp"
#include "sha256.hpp"
#include "util.hpp"
#include <cstring>
#include <fstream>

namespace md {
namespace {
constexpr char CP_MAGIC[8] = {'M','D','C','H','K','P','0','1'};

void put_str(std::vector<uint8_t>& out, const std::string& s) {
    put_u32(out, uint32_t(s.size()));
    out.insert(out.end(), s.begin(), s.end());
}
std::string get_str(const uint8_t* p, size_t& off, size_t total) {
    if (off + 4 > total) return {};
    uint32_t n = get_u32(p + off); off += 4;
    if (off + n > total) return {};
    std::string s(reinterpret_cast<const char*>(p + off), n);
    off += n;
    return s;
}
} // namespace

const char* stage_name(PipelineStage s) noexcept {
    static const char* names[] = {"source","timeline","segment","silence",
                                  "fingerprint","complete"};
    return names[uint8_t(s)];
}

std::array<uint8_t, 32> hash_config(const ProcessConfig& cfg) {
    Sha256 h;
    auto feed_u32 = [&](uint32_t v) {
        uint8_t b[4]; for (int i=0;i<4;++i) b[i]=uint8_t(v>>(8*i)); h.update(b,4);
    };
    auto feed_i64 = [&](int64_t v) {
        uint8_t b[8]; for (int i=0;i<8;++i) b[i]=uint8_t(v>>(8*i)); h.update(b,8);
    };
    // 覆盖所有影响输出的参数；路径类字段不参与（不影响媒体结果）
    h.update(reinterpret_cast<const uint8_t*>(&PARAM_VERSION), 4);
    feed_u32(cfg.queue_capacity); feed_u32(cfg.reorder_window);
    feed_u32(cfg.max_frame_cache); feed_i64(cfg.min_segment_us);
    feed_i64(cfg.silence_split_us); feed_u32(cfg.silence_window_ms);
    feed_u32(cfg.silence_overlap_ms); feed_u32(uint32_t(cfg.silence_threshold_milli));
    feed_i64(cfg.drift_limit_us);
    uint8_t flags = (cfg.resume ? 1u : 0u) | (cfg.aac ? 2u : 0u);
    h.update(&flags, 1);
    return h.finish();
}

bool save_checkpoint(const std::string& path, const CheckpointData& cp,
                     PipelineError& err) {
    std::vector<uint8_t> buf;
    buf.insert(buf.end(), CP_MAGIC, CP_MAGIC + 8);
    put_u32(buf, cp.param_version);
    buf.insert(buf.end(), cp.input_sha256.begin(), cp.input_sha256.end());
    buf.insert(buf.end(), cp.params_sha256.begin(), cp.params_sha256.end());
    buf.push_back(uint8_t(cp.stage));
    put_u32(buf, cp.source_cursor);
    put_u64(buf, cp.timeline_cursor);
    put_str(buf, cp.recovered_stage);
    put_u32(buf, uint32_t(cp.segments.size()));
    for (const auto& s : cp.segments) {
        put_u32(buf, s.id);
        put_str(buf, s.sha256_hex);
        put_u64(buf, uint64_t(s.begin_us));
        put_u64(buf, uint64_t(s.end_us));
        put_u32(buf, s.packets);
    }
    std::string io_err;
    if (!atomic_write(path, buf, io_err)) {
        err = {ErrType::DiskWriteFailed, stage::Checkpoint, 0, io_err};
        return false;
    }
    return true;
}

std::optional<CheckpointData> load_checkpoint(const std::string& path,
                                              PipelineError& err) {
    std::ifstream f(path, std::ios::binary);
    if (!f) return std::nullopt; // 首次运行：无检查点
    std::vector<uint8_t> buf((std::istreambuf_iterator<char>(f)),
                             std::istreambuf_iterator<char>());
    if (buf.size() < 8 || std::memcmp(buf.data(), CP_MAGIC, 8) != 0) {
        err = {ErrType::CheckpointMismatch, stage::Checkpoint, 0,
               "checkpoint corrupt: bad magic"};
        return std::nullopt;
    }
    size_t off = 8, total = buf.size();
    const uint8_t* p = buf.data();
    CheckpointData cp;
    cp.param_version = get_u32(p + off); off += 4;
    std::memcpy(cp.input_sha256.data(), p + off, 32); off += 32;
    std::memcpy(cp.params_sha256.data(), p + off, 32); off += 32;
    cp.stage = PipelineStage(p[off++]);
    cp.source_cursor = get_u32(p + off); off += 4;
    cp.timeline_cursor = get_u64(p + off); off += 8;
    cp.recovered_stage = get_str(p, off, total);
    uint32_t nseg = get_u32(p + off); off += 4;
    for (uint32_t i = 0; i < nseg; ++i) {
        SegmentCheckpoint s;
        s.id = get_u32(p + off); off += 4;
        s.sha256_hex = get_str(p, off, total);
        s.begin_us = int64_t(get_u64(p + off)); off += 8;
        s.end_us = int64_t(get_u64(p + off)); off += 8;
        s.packets = get_u32(p + off); off += 4;
        cp.segments.push_back(std::move(s));
    }
    if (off != total) {
        err = {ErrType::CheckpointMismatch, stage::Checkpoint, 0,
               "checkpoint corrupt: trailing bytes"};
        return std::nullopt;
    }
    return cp;
}

CpReject validate_checkpoint(const CheckpointData& cp,
                             const std::array<uint8_t, 32>& input_sha,
                             const std::array<uint8_t, 32>& params_sha) {
    if (cp.param_version != PARAM_VERSION) return CpReject::ParamVersion;
    if (cp.input_sha256 != input_sha) return CpReject::InputChanged;
    if (cp.params_sha256 != params_sha) return CpReject::ParamsChanged;
    return CpReject::None;
}

const char* cp_reject_reason(CpReject r) noexcept {
    switch (r) {
        case CpReject::None: return "ok";
        case CpReject::Missing: return "checkpoint missing";
        case CpReject::ParamVersion: return "parameter version mismatch";
        case CpReject::InputChanged: return "input summary mismatch";
        case CpReject::ParamsChanged: return "parameters changed";
        case CpReject::Corrupt: return "checkpoint corrupt";
    }
    return "unknown";
}

} // namespace md
