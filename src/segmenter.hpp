#pragma once
// Segmenter: cuts one track's ordered packet stream into timeline-aligned
// segments. Keyframes anchor safe video start points; missing index pages,
// timestamp regressions and truncated tails trigger a local index rebuild by
// scanning decodable packets. Segments without a safe start keep gap metadata.
#include <map>
#include <vector>

#include "capture.hpp"
#include "common.hpp"

namespace mb {

struct Segment {
  u32 segment_id = 0;          // globally unique, assigned in deterministic order
  u32 track_id = 0;
  u32 ssrc = 0;
  i64 start_ns = 0;
  i64 end_ns = 0;
  u64 source_offset_begin = 0; // byte range inside the capture file
  u64 source_offset_end = 0;
  u32 first_seq = 0;
  u32 last_seq = 0;
  bool keyframe_hit = false;   // segment starts at a real keyframe packet
  bool index_rebuilt = false;  // local scan replaced a missing index page
  bool truncated = false;      // tail segment cut by end of capture
  bool has_gap = false;        // contains an unrecoverable packet gap
  std::vector<u8> normalized;  // canonical bytes used for hashing/dedup
  std::vector<ErrorRecord> errors;
};

struct SegmenterResult {
  std::vector<Segment> segments;
  u64 keyframe_index_hits = 0;
  u64 keyframe_index_misses = 0;
  u64 rebuilt_pages = 0;
};

class Segmenter {
 public:
  Segmenter(TrackInfo track, bool capture_truncated, bool index_missing);

  // Index pages as stored in the capture (may be empty when pages missing).
  void load_index_pages(const std::vector<RawRecord>& pages);

  // Run the cut over one track's mapped packets.
  SegmenterResult run(const std::vector<PacketRecord>& packets);

 private:
  struct KeyIndex {
    u32 rtp_ts;
    u64 byte_offset;
  };
  void rebuild_index(const std::vector<PacketRecord>& packets);
  bool has_keyframe_near(i64 target_ns, const PacketRecord& head) const;

  TrackInfo track_;
  bool capture_truncated_;
  bool index_missing_;
  std::vector<KeyIndex> index_;
};

}  // namespace mb
