#pragma once
// RTP ingest: per-SSRC jitter buffer that reorders by sequence number,
// counts loss / duplicates / reorder depth, handles 16-bit wrap, and marks
// late packets outside the reorder window as unrecoverable gaps.
#include <map>
#include <optional>
#include <vector>

#include "capture.hpp"
#include "common.hpp"

namespace mb {

struct IngestStats {
  u32 ssrc = 0;
  u64 received = 0;          // valid RTP datagrams accepted
  u64 duplicates = 0;
  u64 lost = 0;              // gaps that left the reorder window
  u64 late_unrecoverable = 0;
  u64 corrupt = 0;
  u64 invalid = 0;           // oversized / malformed datagrams
  u32 max_reorder_depth = 0;
  std::optional<u64> first_abs_seq;
  std::optional<u64> last_abs_seq;
};

struct IngestResult {
  std::vector<PacketRecord> packets;  // per-SSRC ordered, tracks concatenated
  std::vector<RawRecord> rtcp;        // SR records in arrival order
  std::map<u32, IngestStats> stats;
  std::vector<ErrorRecord> errors;
};

class RtpIngest {
 public:
  explicit RtpIngest(const std::vector<TrackInfo>& tracks);

  // Feed one container record. Track declarations and index pages are
  // ignored here (handled by pipeline / segmenter).
  void push(const RawRecord& rec);

  // Flush all jitter buffers at end of stream; remaining gaps become errors.
  // capture_size is used as the byte offset attached to tail-gap errors.
  IngestResult finish(u64 capture_size);

 private:
  struct Buffer {
    explicit Buffer(TrackInfo t) : track(std::move(t)) {}
    TrackInfo track;
    std::map<u64, PacketRecord> pending;  // abs seq -> packet
    std::optional<u32> last_seq;          // highest raw seq observed
    u64 high_water = 0;                   // highest abs seq inserted
    std::optional<u64> next_to_emit;
    IngestStats stats{};
  };
  Buffer& buffer_for(u16 track_id, u32 ssrc);
  void emit_ready(Buffer& b, std::vector<PacketRecord>& out);
  void push_rtp(u16 track_id, const RawRecord& rec);

  std::map<u16, Buffer> buffers_;
  std::map<u32, u16> ssrc_to_track_;
  std::vector<RawRecord> rtcp_;
  std::vector<ErrorRecord> errors_;
};

}  // namespace mb
