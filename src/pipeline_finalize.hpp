// pipeline_finalize.hpp — 流水线收尾：指标汇总、JSON 原子提交与最终检查点
#pragma once
#include "common.hpp"
#include "checkpoint.hpp"
#include "packet_source.hpp"
#include "segmenter.hpp"
#include "timeline.hpp"

namespace md {

// 汇总运行指标、原子写交付 JSON 并把检查点推进到 Complete。
// 返回 Ok 或 CorruptInput（输入损坏但已生成可追溯结果）。
ExitCode finalize_delivery(
    const ProcessConfig& cfg, PipelineResult& result, CheckpointData& cp,
    const PacketSource& src, const Segmenter& segmenter,
    const TimelineNormalizer& tn,
    const std::vector<PipelineError>& norm_errors,
    uint32_t total_records, uint32_t handled_records,
    uint32_t kf_indexed_ok, uint32_t kf_total);

} // namespace md
