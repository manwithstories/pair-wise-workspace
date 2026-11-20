#include "relay/transport_reorder.hpp"

namespace relay {

void TransportReorderer::emit(StreamState& st, const PacketRecord& p,
                              std::vector<PacketRecord>& emitted) {
  st.last_ts_ms = p.ts_ms;
  st.next = p.seq + 1;
  emitted.push_back(p);
}

void TransportReorderer::record_gap(uint64_t first, uint64_t last,
                                    int64_t start_ms, int64_t end_ms) {
  if (last < first) return;
  SeqGap g;
  g.first_seq = first;
  g.last_seq = last;
  g.start_ms = start_ms;
  g.end_ms = end_ms;
  uint64_t len = last - first + 1;
  g.burst = len >= burst_threshold_;
  stats_.lost_packets += len;
  if (g.burst) ++stats_.burst_losses;
  gaps_.push_back(g);
}

ReorderEvent TransportReorderer::push(const PacketRecord& p,
                                      std::vector<PacketRecord>& emitted) {
  StreamState& st = streams_[{p.node_id, p.stream_id}];
  if (!st.started) {
    st.started = true;
    st.next = p.seq;
  }

  if (p.seq < st.next) {
    // Behind the cursor: either a retransmission of something we emitted
    // inside the window, or a packet too late to place. The late case is
    // recorded as a one-packet gap rather than silently re-sequenced.
    if (p.seq + window_ > st.next) {
      ++stats_.duplicate_packets;
      return ReorderEvent::Duplicate;
    }
    ++stats_.late_dropped;
    record_gap(p.seq, p.seq, p.ts_ms, p.ts_ms);
    return ReorderEvent::LateDropped;
  }

  if (st.buf.count(p.seq) != 0) {
    ++stats_.duplicate_packets;
    return ReorderEvent::Duplicate;
  }

  if (p.seq == st.next) {
    emit(st, p, emitted);
    while (!st.buf.empty() && st.buf.begin()->first == st.next) {
      PacketRecord q = st.buf.begin()->second;
      st.buf.erase(st.buf.begin());
      emit(st, q, emitted);
    }
    return ReorderEvent::InOrder;
  }

  // Ahead of expectation: hold inside the reorder window.
  ++stats_.reordered_packets;
  st.buf.emplace(p.seq, p);
  if (st.buf.size() >= window_) {
    // Window full: the missing prefix is declared lost. Record the gap with
    // its time bounds; do not fabricate ordering for what never arrived.
    uint64_t first_missing = st.next;
    uint64_t last_missing = st.buf.begin()->first - 1;
    int64_t gap_end_ms = st.buf.begin()->second.ts_ms;
    record_gap(first_missing, last_missing, st.last_ts_ms, gap_end_ms);
    st.next = st.buf.begin()->first;
    while (!st.buf.empty() && st.buf.begin()->first == st.next) {
      PacketRecord q = st.buf.begin()->second;
      st.buf.erase(st.buf.begin());
      emit(st, q, emitted);
    }
  }
  return ReorderEvent::Reordered;
}

void TransportReorderer::flush(std::vector<PacketRecord>& emitted) {
  for (auto& [key, st] : streams_) {
    (void)key;
    if (!st.started) continue;
    // Close out the window: every hole before a buffered packet is a gap.
    while (!st.buf.empty()) {
      record_gap(st.next, st.buf.begin()->first - 1, st.last_ts_ms,
                 st.buf.begin()->second.ts_ms);
      st.next = st.buf.begin()->first;
      PacketRecord q = st.buf.begin()->second;
      st.buf.erase(st.buf.begin());
      emit(st, q, emitted);
    }
  }
}

}  // namespace relay
