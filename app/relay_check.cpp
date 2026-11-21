#include "relay/stream_pipeline.hpp"

#include <cinttypes>
#include <cstdio>
#include <cstring>
#include <string>

using namespace relay;

namespace {

struct Args {
  std::string input;
  std::string output;
  size_t queue_capacity = 64;
  uint32_t segment_frames = 120;
};

bool parse_args(int argc, char** argv, Args& a) {
  for (int i = 1; i < argc; ++i) {
    auto need = [&](const char* v) -> const char* { return v; };
    if (std::strcmp(argv[i], "--input") == 0 && i + 1 < argc)
      a.input = need(argv[++i]);
    else if (std::strcmp(argv[i], "--output") == 0 && i + 1 < argc)
      a.output = need(argv[++i]);
    else if (std::strcmp(argv[i], "--queue-capacity") == 0 && i + 1 < argc)
      a.queue_capacity = std::stoull(argv[++i]);
    else if (std::strcmp(argv[i], "--segment-frames") == 0 && i + 1 < argc)
      a.segment_frames = static_cast<uint32_t>(std::stoul(argv[++i]));
    else {
      std::fprintf(stderr, "unknown or incomplete argument: %s\n", argv[i]);
      return false;
    }
  }
  if (a.input.empty() || a.output.empty() || a.queue_capacity == 0 ||
      a.segment_frames == 0) {
    std::fprintf(stderr,
                 "usage: relay_check --input <frames> --output <dir> "
                 "[--queue-capacity N] [--segment-frames N]\n");
    return false;
  }
  return true;
}

// Re-reads every completed segment, verifying checksums and decoding each
// frame. Returns the number of frames that pass roundtrip verification.
uint64_t verify_roundtrip(const SegmentWriter& writer) {
  uint64_t verified = 0;
  for (const auto& seg : writer.completed()) {
    if (!SegmentWriter::verify_file(seg.path, seg.sha256, seg.bytes)) {
      std::fprintf(stderr, "segment hash mismatch: %s\n", seg.path.c_str());
      continue;
    }
    std::FILE* f = std::fopen(seg.path.c_str(), "rb");
    if (!f) continue;
    while (true) {
      uint8_t lenbuf[4];
      if (std::fread(lenbuf, 1, 4, f) != 4) break;
      uint32_t len = 0;
      for (int i = 0; i < 4; ++i)
        len |= static_cast<uint32_t>(lenbuf[i]) << (i * 8);
      if (len < 68 || len > 64u * 1024u * 1024u) break;
      std::vector<uint8_t> raw(len);
      std::memcpy(raw.data(), lenbuf, 4);
      if (std::fread(raw.data() + 4, 1, len - 4, f) != len - 4) break;
      FrameRecord rec;
      if (!parse_record(raw.data(), raw.size(), rec).ok()) break;
      if (!checksum_matches(rec)) continue;
      if (decode_validate(rec.payload).ok) ++verified;
    }
    std::fclose(f);
  }
  return verified;
}

} // namespace

int main(int argc, char** argv) {
  Args args;
  if (!parse_args(argc, argv, args)) return 2;

  PipelineConfig cfg;
  cfg.queue_capacity = args.queue_capacity;
  cfg.segment_frames = args.segment_frames;
  cfg.clock.wrap_modulus = int64_t{1} << 33;

  StreamPipeline pipeline(cfg, args.output);
  PipelineStatus status = pipeline.run(args.input);
  const PipelineStats& st = pipeline.stats();

  if (status == PipelineStatus::InputError) {
    std::fprintf(stderr, "error=%s first_error_offset=%llu\n",
                 to_string(pipeline.structural_error().error),
                 (unsigned long long)pipeline.structural_error().error_offset);
    return 2;
  }
  if (status == PipelineStatus::MediaError) {
    std::printf("error=%s first_error_offset=%llu\n",
                to_string(pipeline.structural_error().error),
                (unsigned long long)pipeline.structural_error().error_offset);
    return 3;
  }
  if (status == PipelineStatus::WriteError) {
    std::fprintf(stderr, "error=write_or_recovery_failed\n");
    return 4;
  }

  // Roundtrip + index verification.
  const uint64_t verified = verify_roundtrip(pipeline.writer());
  const double consistency =
      st.input_records > 0
          ? 100.0 * static_cast<double>(verified) /
                static_cast<double>(st.input_records)
          : 100.0;
  const double hit_rate = 100.0 * pipeline.index().keyframe_hit_rate();

  // Read-path index validation: offset bounds + keyframe hits.
  uint64_t index_failures = 0;
  for (const auto& e : pipeline.index().entries()) {
    uint64_t seg_bytes = 0;
    for (const auto& seg : pipeline.writer().completed())
      if (seg.id == e.segment_id) seg_bytes = seg.bytes;
    if (seg_bytes == 0 || !pipeline.index().validate_entry(e, seg_bytes))
      ++index_failures;
  }

  std::printf("roundtrip_consistency=%.1f%%\n", consistency);
  std::printf("keyframe_index_hit_rate=%.1f%%\n", hit_rate);
  std::printf("segments=%zu\n", pipeline.writer().completed().size());
  for (const auto& seg : pipeline.writer().completed())
    std::printf("segment_sha256[%05u]=%s\n", seg.id, hex(seg.sha256).c_str());
  std::printf("peak_buffered_frames=%zu\n", st.peak_buffered_frames);
  std::printf("repaired_wraps=%llu\n", (unsigned long long)st.repaired_wraps);
  std::printf("isolated_corrupt_frames=%llu\n",
              (unsigned long long)(st.checksum_isolated + st.decode_failed));
  std::printf("duplicates=%llu\n", (unsigned long long)st.duplicates);
  std::printf("timeline_gaps=%llu\n", (unsigned long long)st.gaps);
  std::printf("clock_errors=%llu\n", (unsigned long long)st.clock_errors);
  std::printf("frames_written=%llu\n", (unsigned long long)st.frames_written);
  std::printf("backpressure_events=%llu\n",
              (unsigned long long)st.backpressure_events);
  std::printf("parse_rejected=%llu\n", (unsigned long long)st.parse_rejected);

  if (st.peak_buffered_frames > args.queue_capacity) {
    std::fprintf(stderr, "queue capacity exceeded\n");
    return 4;
  }
  if (index_failures > 0 || hit_rate < 100.0) {
    std::fprintf(stderr, "index validation failed: %llu\n",
                 (unsigned long long)index_failures);
    return 3;
  }
  return 0;
}
