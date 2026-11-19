#include "pipeline_finalize.hpp"
#include "util.hpp"
#include <algorithm>
#include <sstream>

namespace md {

ExitCode finalize_delivery(
    const ProcessConfig& cfg, PipelineResult& result, CheckpointData& cp,
    const PacketSource& src, const Segmenter& segmenter,
    const TimelineNormalizer& tn,
    const std::vector<PipelineError>& norm_errors,
    uint32_t total_records, uint32_t handled_records,
    uint32_t kf_indexed_ok, uint32_t kf_total) {
    (void)segmenter;
    // ---- 指标汇总 ----
    result.errors.insert(result.errors.end(), norm_errors.begin(),
                        norm_errors.end());
    result.source_packets = total_records;
    result.delivered_packets = handled_records;
    result.lost_packets = tn.counters().losses;
    result.duplicate_packets += tn.counters().duplicates;
    result.untrusted_spans = tn.counters().untrusted_spans;
    result.segment_count = uint32_t(result.segments.size());
    result.keyframe_hit_rate = kf_total
        ? double(kf_indexed_ok) / double(kf_total) : 1.0;
    // 干净流：完整读取记录数 == 头部声明数 => 1.0；
    // 截断流：实际可完整读取数 < 声明数 => <1.0
    uint32_t consumed = total_records + uint32_t(src.rtcp_reports().size());
    uint32_t denom = src.expected_records() ? src.expected_records() : consumed;
    result.roundtrip_rate = denom
        ? double(std::min(consumed, denom)) / double(denom) : 1.0;

    // ---- 原子提交最终交付 JSON ----
    std::vector<uint8_t> out_bytes;
    std::ostringstream oss;
    write_result_json(oss, result, cfg);
    auto s = oss.str();
    out_bytes.assign(s.begin(), s.end());
    std::string io_err;
    if (!atomic_write(cfg.out_path, out_bytes, io_err)) {
        result.errors.push_back(
            {ErrType::DiskWriteFailed, stage::Checkpoint, 0, io_err});
        return ExitCode::IoError;
    }
    cp.stage = PipelineStage::Complete;
    cp.source_packets = result.source_packets;
    cp.delivered_packets = result.delivered_packets;
    cp.lost_packets = result.lost_packets;
    cp.duplicate_packets = result.duplicate_packets;
    cp.untrusted_spans = result.untrusted_spans;
    cp.peak_queue_items = result.peak_queue_items;
    cp.duplicate_count = result.duplicate_count;
    cp.roundtrip_rate = result.roundtrip_rate;
    cp.keyframe_hit_rate = result.keyframe_hit_rate;
    cp.errors = result.errors;
    cp.recovered_stage = result.recovered_stage;
    PipelineError cerr;
    save_checkpoint(cfg.checkpoint_path, cp, cerr);

    bool corrupt = std::any_of(result.errors.begin(), result.errors.end(),
        [](const PipelineError& e) {
            return e.type == ErrType::TruncatedRecord ||
                   e.type == ErrType::DecodeError;
        });
    return corrupt ? ExitCode::CorruptInput : ExitCode::Ok;
}

} // namespace md
