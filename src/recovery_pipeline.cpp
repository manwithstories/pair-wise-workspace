#include <algorithm>

#include "media_archive/audio_sync.h"
#include "media_archive/keyframe_index.h"
#include "media_archive/packet_reader.h"
#include "media_archive/recovery_pipeline.h"
#include "media_archive/sequence_repair.h"
#include "media_archive/timestamp_repair.h"
#include "media_archive/transport_trace.h"

namespace media_archive {

Diagnostic verify_index_file(const std::string& index_path, CodecType codec,
                             const std::vector<uint64_t>& sample_pts,
                             double* max_seek_error_ms) {
  std::vector<KeyframeEntry> entries;
  IndexFileHeader hdr{};
  Diagnostic d = read_keyframe_index(index_path, codec, &entries, &hdr);
  if (!d.ok()) return d;
  double worst = 0.0;
  for (uint64_t pts : sample_pts)
    worst = std::max(worst, verify_seek(entries, pts).error_ms);
  *max_seek_error_ms = worst;
  return {};
}

Diagnostic run_recovery_pipeline(const RecoveryOptions& opt,
                                 PipelineResult* out) {
  RecoveryReport& rep = out->report;
  rep.codec = opt.codec;

  // 1. Load the received elementary stream under the size/offset limits.
  std::vector<uint8_t> stream;
  Diagnostic d = read_file_bounded(opt.input_path, opt.limits, &stream);
  if (!d.ok()) return d;

  // 2. Parse the codec explicitly; corruption carries a byte offset.
  std::vector<ParsedFrame> frames;
  d = parse_elementary_stream(stream, opt.codec, opt.limits, &frames);
  if (!d.ok()) {
    rep.failure_offset = d.offset;
    return d;
  }

  // 3. Read the transport manifest and repair the sequence.
  std::vector<TransportPacket> manifest;
  d = parse_packet_manifest(opt.packets_path, &manifest);
  if (!d.ok()) return d;
  // Count the present packets before dedup: every present packet's frame
  // bytes were physically written into the received stream, so the parser
  // must yield exactly this many frames. Gaps and dedup are excluded here.
  const size_t present_in_stream = std::count_if(
      manifest.begin(), manifest.end(),
      [](const TransportPacket& p) { return p.flag != PacketFlag::Gap; });
  SequenceResult seq = repair_sequence(manifest);

  // A truncated elementary stream yields fewer frames than the manifest's
  // present packets. Detect it explicitly and point at the stream tail.
  if (frames.size() != present_in_stream) {
    const int64_t tail = static_cast<int64_t>(stream.size());
    rep.failure_offset = tail;
    return diag(Status::MediaCorrupt,
                "truncated stream: parsed " +
                    std::to_string(frames.size()) +
                    " frames but manifest declares " +
                    std::to_string(present_in_stream) + " present packets",
                tail);
  }

  rep.recovered_packets = seq.ordered.size();
  rep.unrecovered_bytes = seq.unrecovered_bytes;
  rep.gap_count = seq.gaps.size();
  // Recoverable set = packets actually present; the recovery rate is the
  // present share of all unique sequence numbers (present + gap slots).
  const uint64_t gap_packets =
      seq.expected_unique > seq.ordered.size()
          ? seq.expected_unique - seq.ordered.size()
          : 0;
  const uint64_t total_unique = seq.ordered.size() + gap_packets;
  rep.loss_recovery_rate =
      total_unique == 0
          ? 1.0
          : static_cast<double>(seq.ordered.size()) /
                static_cast<double>(total_unique);

  // 4. Measure latency p95 from the trace carried in the manifest.
  std::vector<double> latencies;
  for (const auto& p : manifest)
    if (p.flag != PacketFlag::Gap && p.recv_ts_ms > 0.0)
      latencies.push_back(p.recv_ts_ms - p.send_ts_ms);
  rep.p95_latency_ms = percentile(latencies, 0.95);
  rep.p95_sample_count = latencies.size();

  // 5. Timestamp repair with per-packet preceding-gap lengths.
  TimestampConfig tcfg;
  tcfg.drift_ppm = opt.drift_ppm;
  TimestampResult ts =
      repair_timestamps(seq.ordered, seq.gap_before, tcfg);
  rep.uncertain_pts_count = ts.uncertain_count;

  // 6. Rebuild the random-access keyframe index.
  std::vector<KeyframeEntry> entries = build_keyframe_index(
      frames, ts.packets, stream, stream.size());
  rep.keyframe_count = entries.size();

  // 7. Persist through the chunked, resumable checkpoint store.
  CheckpointStore store;
  d = store.open(opt.checkpoint_dir, opt.checkpoint);
  if (!d.ok()) {
    out->checkpoint_failure = true;
    return d;
  }
  out->checkpoint.resumed = store.blocks_written() > 0;
  rep.checkpoint_resumed = out->checkpoint.resumed;
  rep.rolled_back_block = store.rolled_back_block();
  // On resume, records already persisted in earlier blocks are skipped rather
  // than appended again, keeping the rebuilt index identical to a clean run.
  const uint64_t already_durable = store.durable_records();
  for (uint64_t i = 0; i < entries.size(); ++i) {
    if (i < already_durable) continue;
    d = store.append(entries[i], opt.crash_after_index_appends);
    if (!d.ok()) {
      out->checkpoint_failure = true;
      return d;
    }
  }
  d = store.flush();
  if (!d.ok()) {
    out->checkpoint_failure = true;
    return d;
  }

  // 8. Publish the standalone index file and verify seek accuracy.
  d = write_keyframe_index(opt.index_path, opt.codec, entries, stream.size());
  if (!d.ok()) return d;
  std::vector<uint64_t> seek_samples;
  for (size_t i = 0; i < entries.size(); i += 4)
    seek_samples.push_back(entries[i].pts_ticks);
  if (seek_samples.empty() && !entries.empty())
    seek_samples.push_back(entries.front().pts_ticks);
  d = verify_index_file(opt.index_path, opt.codec, seek_samples,
                        &rep.index_seek_error_ms);
  if (!d.ok()) return d;

  // 10. Align audio on the shared clock and measure A/V sync.
  AudioArchive audio;
  d = read_audio_archive(opt.audio_path, &audio);
  if (!d.ok()) return d;
  // The timeline runs one frame interval beyond the last frame's PTS.
  uint64_t timeline_end = 0;
  if (!ts.packets.empty()) {
    timeline_end = ts.packets.back().repaired_pts;
    if (ts.nominal_frame_ticks > 0)
      timeline_end += ts.nominal_frame_ticks;
  }
  AudioSyncResult av =
      align_audio(audio, kCommonAudioRate, timeline_end);
  // A/V sync error is the worst fragment placement error on the shared
  // clock; the sample-count difference is reported separately rather than
  // conflated with timing.
  rep.av_sync_error_ms = av.max_sync_error_ms;
  rep.audio_sample_delta = av.audio_sample_delta;

  rep.valid = true;
  return {};
}

}  // namespace media_archive
