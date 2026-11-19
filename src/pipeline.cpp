#include "media/pipeline.hpp"

#include "media/av_sync.hpp"
#include "media/checkpoint_index.hpp"
#include "media/jitter_buffer.hpp"
#include "media/packet_reorder.hpp"
#include "media/recovery.hpp"
#include "media/subtitle.hpp"
#include "media/timebase.hpp"

#include <algorithm>
#include <map>
#include <vector>

namespace media {

PipelineResult run_pipeline(const DemoConfig& cfg, const DemoTrace& trace,
                            ErrorModel& errors,
                            uint64_t stop_after_arrivals) {
    PipelineResult res;
    ManifestData& m = res.manifest;
    m.target_kbps = cfg.target_kbps;

    CheckpointIndex idx(std::filesystem::path(cfg.output_dir) / "checkpoints",
                        cfg.checkpoint_packets);
    idx.load(errors);
    res.resumed = idx.chunk_count() > 0;
    m.checkpoint_resume = res.resumed;
    const uint64_t resume_seq = idx.resume_sequence();

    PacketReorder reorder(64);
    JitterBuffer jitter(2000, 8, 256, 120);
    RecoveryStats rstats;
    AvSync avsync;

    std::map<uint32_t, std::vector<MediaPacket>> group_data;
    std::map<uint32_t, uint32_t> group_parity;
    std::vector<MediaPacket> outbuf;
    uint32_t open_group =
        static_cast<uint32_t>(resume_seq / kFecDataPerGroup);
    const uint32_t max_group =
        static_cast<uint32_t>((trace.total_data_packets + kFecDataPerGroup - 1) /
                              kFecDataPerGroup);
    int64_t last_video_pts = -1;
    bool write_failed = false;

    auto group_size = [&](uint32_t g) -> uint32_t {
        uint64_t start = static_cast<uint64_t>(g) * kFecDataPerGroup;
        uint64_t remain = trace.total_data_packets - start;
        return static_cast<uint32_t>(std::min<uint64_t>(remain, kFecDataPerGroup));
    };

    // 关闭 [open_group, g) 的组：恢复并汇入输出缓冲（整组追加）。
    auto close_groups_up_to = [&](uint32_t g) {
        while (open_group < g && open_group < max_group) {
            auto it = group_data.find(open_group);
            std::vector<MediaPacket> present;
            if (it != group_data.end()) present = std::move(it->second);
            uint32_t par = group_parity.count(open_group)
                               ? group_parity[open_group]
                               : 0;
            auto rec = recover_group(open_group, present, par,
                                     group_size(open_group), rstats, errors,
                                     "video/main");
            std::sort(present.begin(), present.end(),
                      [](const MediaPacket& a, const MediaPacket& b) {
                          return a.sequence < b.sequence;
                      });
            auto merged = std::move(present);
            merged.insert(merged.end(), rec.recovered.begin(),
                          rec.recovered.end());
            std::sort(merged.begin(), merged.end(),
                      [](const MediaPacket& a, const MediaPacket& b) {
                          return a.sequence < b.sequence;
                      });
            for (const auto& p : merged) {
                if (p.track == TrackKind::Video) {
                    last_video_pts = p.pts_ticks;
                } else if (p.track == TrackKind::Audio && last_video_pts >= 0) {
                    avsync.add_pair(p.pts_ticks, last_video_pts);
                }
            }
            outbuf.insert(outbuf.end(), merged.begin(), merged.end());
            group_data.erase(open_group);
            group_parity.erase(open_group);
            open_group++;
        }
    };

    auto commit_if_full = [&]() {
        if (outbuf.size() >= cfg.checkpoint_packets) {
            if (!idx.commit_chunk(outbuf, errors)) write_failed = true;
            outbuf.clear();
        }
    };

    uint64_t arrivals_done = 0;
    for (const auto& pkt : trace.arrivals) {
        if (arrivals_done++ >= stop_after_arrivals) {
            res.completed = false; // 模拟中断：保留已提交块，不写清单
            res.write_failed = write_failed;
            return res;
        }
        if (pkt.is_parity) {
            if (pkt.fec_group >= open_group) group_parity[pkt.fec_group]++;
            continue;
        }
        if (pkt.sequence < resume_seq) continue; // 已提交块跳过，不重复计入
        jitter.observe(pkt.arrival_ms, pkt.payload_bytes);
        for (auto& p : reorder.push(pkt, errors)) {
            close_groups_up_to(p.fec_group);
            group_data[p.fec_group].push_back(std::move(p));
            commit_if_full();
        }
    }
    for (auto& p : reorder.flush()) group_data[p.fec_group].push_back(std::move(p));
    close_groups_up_to(max_group);
    if (!outbuf.empty())
        if (!idx.commit_chunk(outbuf, errors)) write_failed = true;
    res.completed = true;
    res.write_failed = write_failed;

    // 字幕：SRT 与 WebVTT 均容错解析，重叠区间裁剪。
    auto srt = parse_subtitles(trace.srt_content, SubtitleFormat::Srt,
                               OverlapPolicy::Trim, errors, "subtitle/srt");
    auto vtt = parse_subtitles(trace.vtt_content, SubtitleFormat::WebVtt,
                               OverlapPolicy::Trim, errors, "subtitle/vtt");
    m.subtitle_cues = srt.cues.size() + vtt.cues.size();
    m.subtitle_bad_records = srt.bad_records + vtt.bad_records;

    // 音频重采样演练：44.1kHz -> 48kHz 合成音。
    std::vector<int16_t> tone(4410, 1000);
    if (resample_s16_mono(tone, 44100, 48000).empty())
        errors.record({ErrorKind::Uncertain, "audio/resample", -1, -1, -1,
                       "resampler returned no output"});

    // 汇总指标。
    double duration_s = ticks_to_ms(trace.duration_ticks) / 1000.0;
    m.actual_kbps = static_cast<uint64_t>(
        static_cast<double>(idx.committed_bytes()) * 8.0 / duration_s / 1000.0);
    m.bitrate_delta_pct = bitrate_delta_pct(m.target_kbps, m.actual_kbps);
    m.recovered_packets = rstats.recovered_packets;
    m.duplicate_packets = reorder.duplicates();
    m.reordered_packets = reorder.reordered();
    m.lost_packets = rstats.lost_packets;
    m.unrecoverable_packets = rstats.unrecoverable_packets;
    res.unrecoverable = rstats.unrecoverable_packets;
    m.recovery_rate = rstats.recovery_rate();
    m.gaps = rstats.gaps.size();

    auto av = avsync.report();
    m.av_offset_ms = av.offset_ms;
    m.av_drift_ppm = av.drift_ppm;
    m.av_residual_ms = av.residual_error_ms;
    // 清单边界误差：音画残差与字幕毫秒量化上界。
    m.manifest_max_error_ms = std::max(av.residual_error_ms, 1.0);

    double unrecoverable_ratio =
        trace.total_data_packets == 0
            ? 0.0
            : static_cast<double>(rstats.unrecoverable_packets) /
                  trace.total_data_packets;
    m.quality_psnr_db =
        estimate_psnr(static_cast<double>(m.actual_kbps) / m.target_kbps,
                      unrecoverable_ratio);
    m.chunks_committed = idx.chunk_count();
    m.checkpoint_resume = res.resumed;

    if (!write_manifest(std::filesystem::path(cfg.output_dir) / "manifest.json",
                        m, errors))
        res.write_failed = true;
    return res;
}

} // namespace media
