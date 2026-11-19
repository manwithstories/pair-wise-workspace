#include "pipeline.hpp"
#include "pipeline_io.hpp"
#include "pipeline_finalize.hpp"
#include "packet_source.hpp"
#include "timeline.hpp"
#include "segmenter.hpp"
#include "silence.hpp"
#include "fingerprint.hpp"
#include "checkpoint.hpp"
#include "sha256.hpp"
#include "util.hpp"

#include <algorithm>
#include <sstream>
namespace md {
namespace {

// 片段落盘字节：与指纹规范化流一致（统一时间 LE64 + 长度 LE32 + 载荷）
std::vector<uint8_t> canonical_bytes(const std::vector<MediaPacket>& pkts) {
    std::vector<uint8_t> out;
    for (const auto& p : pkts) {
        for (int i = 0; i < 8; ++i)
            out.push_back(uint8_t(uint64_t(p.unified_us) >> (8 * i)));
        put_u32(out, uint32_t(p.payload.size()));
        out.insert(out.end(), p.payload.begin(), p.payload.end());
    }
    return out;
}
void attach_silence(SegmentReport& sr, const ClosedSegment& cs,
                    const std::vector<SilenceInterval>& all) {
    for (const auto& iv : all)
        if (iv.end_us > cs.packets.front().unified_us &&
            iv.begin_us < cs.packets.back().unified_us)
            sr.silence.push_back(iv);
}

} // namespace

ExitCode run_pipeline(const ProcessConfig& cfg, const PipelineHooks& hooks,
                      PipelineResult& result) {
    result = PipelineResult{};
    result.recovered_stage = "none";
    PacketSource src;
    PipelineError src_err;
    if (!src.open(cfg.input_path, src_err)) {
        result.errors.push_back(src_err);
        return src_err.type == ErrType::IoReadFailed
                   ? ExitCode::IoError : ExitCode::BadArgs;
    }
    result.input_sha256 = src.summary().sha256;
    auto params_sha = hash_config(cfg);

    // ---- 检查点兼容性 ----
    CheckpointData cp;
    bool had_cp = false;
    ExitCode compat = checkpoint_compat(cfg, result.input_sha256, params_sha,
                                        cp, had_cp);
    if (compat == ExitCode::IncompatibleCheckpoint) {
        result.errors.push_back(
            {ErrType::CheckpointMismatch, stage::Checkpoint, 0,
             cp_reject_reason(had_cp ? CpReject::None : CpReject::Corrupt)});
        return ExitCode::IncompatibleCheckpoint;
    }
    if (had_cp) result.recovered_stage = stage_name(cp.stage);

    // ---- 恢复：校验已落盘片段，仅信任哈希一致者，其余重算 ----
    std::map<uint32_t, SegmentCheckpoint> trusted;
    if (had_cp) {
        for (const auto& sc : cp.segments) {
            if (verify_segment_blob(cfg, sc, result.errors))
                trusted[sc.id] = sc;
        }
    }

    // ---- Complete 短路：全部片段哈希可信则直接重建输出，绝不重算 ----
    if (had_cp && cp.stage == PipelineStage::Complete &&
        trusted.size() == cp.segments.size() && !cp.segments.empty()) {
        for (const auto& sc : cp.segments) {
            SegmentReport sr;
            sr.id = sc.id;
            sr.sha256 = sc.sha256_hex;
            sr.sample_summary = sc.sample_summary_hex;
            sr.begin_us = sc.begin_us; sr.end_us = sc.end_us;
            sr.packets = sc.packets; sr.keyframes = sc.keyframes;
            sr.keyframe_indexed = sc.keyframe_indexed;
            sr.disposition = Disposition(sc.disposition);
            sr.duplicate_of_other = sc.duplicate_of_other;
            sr.duplicate_of = sc.duplicate_of;
            sr.silence.resize(sc.silence_count);
            result.segments.push_back(std::move(sr));
        }
        result.segment_count = uint32_t(result.segments.size());
        result.duplicate_count = cp.duplicate_count;
        result.keyframe_hit_rate = cp.keyframe_hit_rate;
        result.roundtrip_rate = cp.roundtrip_rate;
        result.peak_queue_items = cp.peak_queue_items;
        result.source_packets = cp.source_packets;
        result.delivered_packets = cp.delivered_packets;
        result.lost_packets = cp.lost_packets;
        result.duplicate_packets = cp.duplicate_packets;
        result.untrusted_spans = cp.untrusted_spans;
        result.errors = cp.errors;
        result.recovered_stage = stage_name(PipelineStage::Complete);
        std::vector<uint8_t> ob;
        std::ostringstream oss;
        write_result_json(oss, result, cfg);
        auto s = oss.str(); ob.assign(s.begin(), s.end());
        std::string io_err;
        if (!atomic_write(cfg.out_path, ob, io_err)) {
            result.errors.push_back(
                {ErrType::DiskWriteFailed, stage::Checkpoint, 0, io_err});
            return ExitCode::IoError;
        }
        return ExitCode::Ok;
    }

    TimelineNormalizer tn(cfg);
    Segmenter segmenter(cfg);
    SilenceDetector silence(cfg);
    Deduper deduper;
    BoundedQueue<MediaPacket> pkt_q(cfg.queue_capacity);
    BoundedQueue<ClosedSegment> seg_q(cfg.queue_capacity);

    std::vector<MediaPacket> norm_batch;
    std::vector<PipelineError> norm_errors;
    bool source_done = false, segment_input_done = false;
    uint32_t seg_id = 0, total_records = 0, handled_records = 0;
    uint32_t kf_total = 0, kf_indexed_ok = 0;
    const uint32_t resume_from = had_cp ? uint32_t(trusted.size()) : 0;

    auto peak = [&] {
        result.peak_queue_items = std::max(
            result.peak_queue_items,
            std::max(pkt_q.size(), seg_q.size()));
    };

    // 片段闭合后的统一处理（指纹 -> 去重 -> 落盘 -> 报告 -> 检查点）
    auto handle_segment = [&](ClosedSegment cs) -> bool {
        if (seg_id < resume_from) { ++seg_id; return true; } // 恢复跳过
        SegmentReport sr;
        sr.id = seg_id;
        sr.begin_us = cs.packets.front().unified_us;
        sr.end_us = cs.packets.back().unified_us;
        sr.packets = uint32_t(cs.packets.size());
        sr.keyframes = cs.starts_with_keyframe ? 1u : 0u;
        sr.keyframe_indexed = cs.starts_with_keyframe;
        sr.disposition = cs.disposition;
        auto blob = canonical_bytes(cs.packets);
        Fingerprint fp = compute_fingerprint(cs.packets);
        sr.sha256 = Sha256::hex(Sha256::hash_bytes(blob));
        sr.sample_summary = fp.sample_summary_hex;
        attach_silence(sr, cs, silence.intervals());

        PipelineError werr;
        if (!write_segment_blob(cfg, hooks, seg_id, blob, werr)) {
            result.errors.push_back(werr);
            return false; // 不可恢复 I/O
        }
        if (auto dup = deduper.check_and_add(sr); dup.has_value()) {
            sr.duplicate_of_other = true;
            sr.duplicate_of = dup.value();
            ++result.duplicate_count;
        }
        result.segments.push_back(std::move(sr));
        kf_indexed_ok += cs.starts_with_keyframe ? 1u : 0u;
        const SegmentReport& saved = result.segments.back(); // move 后从这里取

        // 阶段检查点（原子提交），字段全部取自完整报告
        cp.param_version = PARAM_VERSION;
        cp.input_sha256 = result.input_sha256;
        cp.params_sha256 = params_sha;
        cp.stage = PipelineStage::Segment;
        cp.source_cursor = handled_records;
        cp.timeline_cursor += cs.packets.size();
        cp.segments.push_back({
            seg_id, saved.sha256, saved.sample_summary,
            saved.begin_us, saved.end_us, saved.packets,
            saved.keyframes, saved.keyframe_indexed,
            uint8_t(saved.disposition),
            saved.duplicate_of_other, saved.duplicate_of,
            uint32_t(saved.silence.size())});
        cp.recovered_stage = result.recovered_stage;
        PipelineError cerr;
        if (!save_checkpoint(cfg.checkpoint_path, cp, cerr)) {
            result.errors.push_back(cerr);
            return false;
        }
        if (hooks.crash_after_segment &&
            hooks.crash_after_segment == seg_id + 1)
            throw CrashInjected{seg_id + 1}; // 模拟中断
        ++seg_id;
        return true;
    };

    // ---- 确定性协作式引擎：单线程、固定推进顺序，保证哈希可复现 ----
    try {
        while (true) {
            // 1) 源 -> 时间轴 -> 包队列（背压：队列满则本轮不读源）
            if (!source_done && pkt_q.size() < cfg.queue_capacity) {
                MediaPacket raw;
                if (src.next_rtp(raw,
                        [&](const RtcpReport& rr) {
                            tn.apply_rtcp(rr, norm_errors);
                        }, src_err)) {
                    ++total_records;
                    if (raw.codec == CodecId::H264) {
                        norm_batch.clear();
                        tn.push(raw, norm_batch, norm_errors);
                        for (auto& np : norm_batch) {
                            pkt_q.try_push(std::move(np));
                            ++handled_records;
                        }
                    } else {
                        silence.push(raw, norm_errors); // 音频直达静音阶段
                        ++handled_records;
                    }
                } else if (src_err.type != ErrType::None) {
                    source_done = true; // 截断等：产出可追溯结果后结束
                    result.errors.push_back(src_err);
                } else {                    source_done = true;
                    tn.flush(norm_batch, norm_errors);
                    for (auto& np : norm_batch) {
                        pkt_q.try_push(std::move(np));
                        ++handled_records;
                    }
                    silence.finish();
                }
            }
            peak();

            // 2) 包队列 -> 切分 -> 片段队列
            MediaPacket vp;
            while (pkt_q.try_pop(vp)) {
                if (vp.duplicate) { ++result.duplicate_packets; continue; }
                segmenter.push_video(vp, silence.intervals(),
                    [&](ClosedSegment c) { seg_q.try_push(std::move(c)); });
            }
            if (source_done && !segment_input_done) {
                segment_input_done = true;
                segmenter.finish(silence.intervals(),
                    [&](ClosedSegment c) { seg_q.try_push(std::move(c)); });
            }
            peak();

            // 3) 片段队列 -> 指纹/去重/落盘/检查点
            ClosedSegment cs;
            bool any = false;
            while (seg_q.try_pop(cs)) {
                any = true;
                if (!handle_segment(std::move(cs)))
                    return ExitCode::IoError;
            }
            peak();
            if (source_done && !any && pkt_q.size() == 0 && seg_q.size() == 0)
                break;
        }
    } catch (const CrashInjected&) {
        // 模拟处理中断：检查点已落盘，交调用方以专用码退出
        result.errors.push_back(
            {ErrType::QueueCancelled, stage::Queue, seg_id,
             "simulated interruption; checkpoint persisted"});
        return ExitCode::IoError;
    }

    // ---- 恢复合并：把检查点中已校验通过的片段重建为报告，与新片段汇总 ----
    if (had_cp) {
        std::vector<SegmentReport> merged;
        for (const auto& sc : cp.segments) {
            if (!trusted.count(sc.id)) continue;
            SegmentReport sr;
            sr.id = sc.id;
            sr.sha256 = sc.sha256_hex;
            sr.sample_summary = sc.sample_summary_hex;
            sr.begin_us = sc.begin_us;
            sr.end_us = sc.end_us;
            sr.packets = sc.packets;
            sr.keyframes = sc.keyframes;
            sr.keyframe_indexed = sc.keyframe_indexed;
            sr.disposition = Disposition(sc.disposition);
            sr.duplicate_of_other = sc.duplicate_of_other;
            sr.duplicate_of = sc.duplicate_of;
            sr.silence.resize(sc.silence_count);
            merged.push_back(std::move(sr));
        }
        for (auto& s : result.segments) merged.push_back(std::move(s));
        std::sort(merged.begin(), merged.end(),
                  [](const SegmentReport& a, const SegmentReport& b) {
                      return a.id < b.id;
                  });
        result.segments = std::move(merged);
        if (!trusted.empty())
            kf_indexed_ok += uint32_t(trusted.size());
    }

    return finalize_delivery(cfg, result, cp, src, segmenter, tn,
                             norm_errors, total_records, handled_records,
                             kf_indexed_ok, kf_total);
}

} // namespace md
