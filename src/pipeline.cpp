#include "media/pipeline.hpp"

#include <algorithm>
#include <iostream>
#include <vector>

#include "media/probe.hpp"
#include "media/timeline.hpp"

namespace mediaqc {

std::string parameter_digest(const PipelineOptions& opts) {
    const std::string param =
        std::to_string(opts.target_rate_hz) + "|" +
        std::to_string(opts.target_channels) + "|" +
        std::to_string(opts.target_bitrate) + "|" +
        std::to_string(opts.keyframe_gap_ms) + "|" +
        (opts.damage_policy == DamagePolicy::Silence ? "silence" : "truncate");
    return CheckpointStore::digest_params(param);
}

namespace {

// Merge the sample-bearing damage ranges into the policy boundaries recorded
// in the manifest; identical for a fresh repack and a resumed run.
std::vector<Segment> policy_boundaries(const std::vector<DamageRecord>& damage) {
    std::vector<Segment> ranges;
    for (const auto& d : damage)
        if (d.end_sample > d.start_sample) ranges.push_back({d.start_sample, d.end_sample});
    std::sort(ranges.begin(), ranges.end(),
              [](const Segment& a, const Segment& b) { return a.start_sample < b.start_sample; });
    std::vector<Segment> merged;
    for (const auto& seg : ranges) {
        if (!merged.empty() && seg.start_sample <= merged.back().end_sample)
            merged.back().end_sample = std::max(merged.back().end_sample, seg.end_sample);
        else
            merged.push_back(seg);
    }
    return merged;
}

}  // namespace

std::optional<TrackManifest> process_track(const InputBlock& block,
                                           const PipelineOptions& opts,
                                           const std::string& validated_out_dir,
                                           CheckpointStore& checkpoint,
                                           const std::string& param_digest,
                                           const fs::StatInfo& st) {
    ScanConfig scan_cfg;    scan_cfg.keyframe_gap_ns = opts.keyframe_gap_ms * 1'000'000LL;
    scan_cfg.frames_per_page = opts.target_rate_hz;

    RepackConfig repack_cfg;
    repack_cfg.target_rate_hz = opts.target_rate_hz;
    repack_cfg.target_channels = opts.target_channels;
    repack_cfg.target_bitrate = opts.target_bitrate;
    repack_cfg.passthrough_tolerance_bps = opts.target_bitrate / 100;  // 1% tolerance
    repack_cfg.damage_policy = opts.damage_policy;

    auto probe = probe_wav_header(block.path);
    load_index(probe);
    auto scan = scan_damage(probe, scan_cfg);
    if (scan.unrecoverable) {
        std::cerr << "unrecoverable: " << block.path << "\n";
        for (const auto& e : scan.damage)
            std::cerr << "  " << to_string(e.code) << " @" << e.source_offset
                      << " " << e.detail << "\n";
        return std::nullopt;
    }

    const std::string out_name = block.id + ".out.wav";
    RepackResult repacked;
    bool reused = false;
    if (opts.resume) {
        if (const auto* kept =
                checkpoint.valid_entry(block.id, st.size, st.mtime_ns, param_digest)) {
            const int bps = probe.header.bits_per_sample;
            const int out_frame_b = opts.target_channels * bps / 8;
            repacked.output_path = kept->output_path;
            repacked.output_bytes = kept->output_bytes;
            repacked.bits_per_sample = bps;
            repacked.output_rate_hz = opts.target_rate_hz;
            repacked.output_channels = opts.target_channels;
            repacked.output_frames = kept->output_bytes / out_frame_b;
            repacked.pcm_reused = probe.header.sample_rate == opts.target_rate_hz &&
                                  probe.header.channels == opts.target_channels;
            repacked.actual_bitrate_bps = repacked.output_frames > 0
                ? static_cast<int64_t>(static_cast<long double>(kept->output_bytes * 8) *
                                       opts.target_rate_hz / repacked.output_frames)
                : 0;
            repacked.bitrate_deviation_pct = opts.target_bitrate > 0
                ? 100.0 *
                      static_cast<double>(repacked.actual_bitrate_bps - opts.target_bitrate) /
                      static_cast<double>(opts.target_bitrate)
                : 0.0;
            reused = true;
        }
    }
    if (!reused) {
        repacked = repack(probe, scan, repack_cfg, validated_out_dir, out_name);
        CheckpointEntry ce;
        ce.track_id = block.id;
        ce.source_path = block.path;
        ce.source_size = st.size;
        ce.source_mtime_ns = st.mtime_ns;
        ce.param_digest = param_digest;
        ce.output_path = repacked.output_path;
        ce.output_relpath = out_name;
        ce.output_bytes = repacked.output_bytes;
        ce.completed = true;
        checkpoint.upsert(std::move(ce));
        checkpoint.save();
    } else {
        std::cout << "resume: track " << block.id << " reused from checkpoint\n";
    }

    const auto levels = measure_levels(repacked.output_path, opts.target_channels);
    TimelineMapper mapper(opts.target_rate_hz);
    SampleClock clk(opts.target_rate_hz);
    std::vector<DamageRecord> probe_errors;
    std::vector<PacketInfo> packets;
    stream_packets(block.path, probe.header,
                   [&](const PacketInfo& p) { packets.push_back(p); },
                   scan.packet_count, scan.total_frames, probe_errors);
    for (const auto& p : packets)
        mapper.map_packet(p.pts_samples, probe.header.sample_rate, p.frame_samples);

    TrackManifest tm;
    tm.track_id = block.id;
    tm.source_path = block.path;
    tm.output_path = repacked.output_path;
    tm.start_ns = clk.samples_to_ns(0);
    tm.end_ns = clk.samples_to_ns(repacked.output_frames);
    tm.packets = scan.packet_count;
    tm.damaged_ranges = static_cast<int64_t>(scan.damage.size());
    tm.truncated_bytes = scan.truncated_bytes;
    tm.target_bitrate = opts.target_bitrate;
    tm.actual_bitrate = repacked.actual_bitrate_bps;
    tm.bitrate_deviation_pct = repacked.bitrate_deviation_pct;
    tm.peak_dbfs = levels.peak_dbfs;
    tm.rms_dbfs = levels.rms_dbfs;
    tm.alignment_error_ns = mapper.max_rounding_ns();
    tm.pcm_reused = repacked.pcm_reused;
    tm.damage_policy =
        opts.damage_policy == DamagePolicy::Silence ? "silence" : "truncate";
    tm.index_rebuilt = scan.index_was_rebuilt;
    tm.damage = scan.damage;
    tm.policy_ranges = policy_boundaries(scan.damage);
    return tm;
}

}  // namespace mediaqc
