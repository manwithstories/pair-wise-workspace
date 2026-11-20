#pragma once
// End-to-end pipeline: capture parse -> RTP reorder -> timeline mapping ->
// track graph scheduling -> segmentation -> silence analysis -> fingerprint
// dedup -> summary. Per-track failures are isolated; outputs are written
// atomically so interrupted batches never look like complete deliveries.
#include <map>
#include <string>
#include <vector>

#include "capture.hpp"
#include "common.hpp"
#include "fingerprint.hpp"
#include "fixture.hpp"
#include "rtp_ingest.hpp"
#include "segmenter.hpp"
#include "silence.hpp"
#include "timeline.hpp"
#include "track_graph.hpp"

namespace mb {

struct PipelineConfig {
  u64 seed = 417;
  bool emit_json = false;
  std::string output_dir;          // empty => dry run in memory
  unsigned max_workers = 4;
  FixtureOptions fixture{};
};

struct TrackOutcome {
  TrackInfo info;
  TaskStatus status = TaskStatus::Pending;
  std::string detail;
  std::vector<Segment> segments;
  SilenceResult silence{};
  u64 packets_in = 0;
};

struct PipelineSummary {
  bool ok = false;
  double roundtrip_ratio = 0.0;
  double keyframe_hit_ratio = 0.0;
  u64 segment_count = 0;
  u64 silence_ms = 0;
  u64 recovered = 0;
  u64 failed = 0;
  u64 total_packets = 0;
  u64 emitted_packets = 0;
  u64 keyframe_hits = 0;
  u64 keyframe_misses = 0;
  std::map<u32, IngestStats> ingest;
  std::vector<TrackOutcome> outcomes;
  DedupResult dedup{};
  std::vector<ErrorRecord> errors;
  std::string json;
  std::vector<std::pair<std::array<u8, 32>, Segment>> segment_rows;
};

class Pipeline {
 public:
  explicit Pipeline(PipelineConfig cfg) : cfg_(std::move(cfg)) {}
  PipelineSummary run(const std::vector<u8>& capture_bytes);

 private:
  TrackOutcome process_track(const TrackInfo& track,
                             const std::vector<PacketRecord>& packets,
                             const std::vector<RawRecord>& index_pages,
                             bool capture_truncated, bool index_missing);
  PipelineConfig cfg_;
};

// Atomic file write: data lands at path.tmp and is renamed only on success.
bool atomic_write(const std::string& path, const std::vector<u8>& bytes);
bool atomic_write_text(const std::string& path, std::string_view text);

}  // namespace mb
