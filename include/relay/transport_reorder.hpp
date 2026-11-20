#pragma once

// Transport reorder layer: per (node, stream) sequence-window dedup,
// out-of-order reassembly and gap marking. Late packets that fall outside
// the window are recorded as gaps, never silently re-sequenced.

#include <cstdint>
#include <map>
#include <optional>
#include <vector>

namespace relay {

struct PacketRecord {
  uint32_t node_id = 0;
  uint32_t stream_id = 0;
  uint64_t seq = 0;
  int64_t ts_ms = 0;        // presentation time on the unified ms axis
  int64_t arrival_ms = 0;   // arrival time for bandwidth estimation
  uint32_t payload_bytes = 0;
};

enum class ReorderEvent {
  InOrder,      // exactly the expected sequence number
  Reordered,    // ahead of expectation; buffered inside the window
  Duplicate,    // already seen (re-emitted or buffered)
  LateDropped,  // behind the window; recorded, not re-sequenced
};

struct SeqGap {
  uint64_t first_seq = 0;
  uint64_t last_seq = 0;    // inclusive
  int64_t start_ms = 0;     // unified-axis time where the gap opens
  int64_t end_ms = 0;       // time of the first packet after the gap
  bool burst = false;       // length >= burst threshold
};

struct ReorderStats {
  uint64_t duplicate_packets = 0;
  uint64_t reordered_packets = 0;
  uint64_t lost_packets = 0;    // sequence numbers never delivered
  uint64_t burst_losses = 0;    // gaps whose length hit the burst threshold
  uint64_t late_dropped = 0;
};

class TransportReorderer {
 public:
  explicit TransportReorderer(size_t window, uint64_t burst_threshold = 3)
      : window_(window == 0 ? 1 : window), burst_threshold_(burst_threshold) {}

  // Feed one packet. In-order packets (including any that become unblocked)
  // are appended to `emitted` in sequence order.
  ReorderEvent push(const PacketRecord& p, std::vector<PacketRecord>& emitted);

  // End of input: close any open window, marking remaining missing sequence
  // numbers as gaps and emitting what is still buffered.
  void flush(std::vector<PacketRecord>& emitted);

  const ReorderStats& stats() const { return stats_; }
  const std::vector<SeqGap>& gaps() const { return gaps_; }

 private:
  struct Key {
    uint32_t node, stream;
    bool operator<(const Key& o) const {
      return node < o.node || (node == o.node && stream < o.stream);
    }
  };
  struct StreamState {
    bool started = false;
    uint64_t next = 0;                    // next expected sequence number
    int64_t last_ts_ms = 0;               // ts of last emitted packet
    std::map<uint64_t, PacketRecord> buf; // out-of-order window
  };

  void emit(StreamState& st, const PacketRecord& p,
            std::vector<PacketRecord>& emitted);
  void record_gap(uint64_t first, uint64_t last, int64_t start_ms,
                  int64_t end_ms);

  size_t window_;
  uint64_t burst_threshold_;
  std::map<Key, StreamState> streams_;
  ReorderStats stats_;
  std::vector<SeqGap> gaps_;
};

}  // namespace relay
