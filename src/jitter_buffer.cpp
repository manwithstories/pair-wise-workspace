#include "media/jitter_buffer.hpp"

#include <algorithm>
#include <queue>
#include <unordered_set>

namespace media {

namespace {

// Min-heap on the stable linear index, which gives global ordering across
// 16-bit sequence wraps. Equal linear indices never coexist after dedup.
struct EntryGreater {
  bool operator()(const JitterBuffer::Entry& a,
                  const JitterBuffer::Entry& b) const {
    return a.packet.linear_index > b.packet.linear_index;
  }
};

using EntryQueue =
    std::priority_queue<JitterBuffer::Entry,
                        std::vector<JitterBuffer::Entry>, EntryGreater>;

} // namespace

// Per-instance storage held behind a unique_ptr so the .cpp does not leak the
// priority_queue comparator into the public header.
struct JitterBuffer::Impl {
  EntryQueue queue;
  std::unordered_set<std::uint64_t> accepted;
};

JitterBuffer::JitterBuffer(const JitterConfig& cfg)
    : cfg_(cfg), impl_(std::make_unique<Impl>()) {
  stats_.target_depth_us = cfg.min_depth_us;
}

JitterBuffer::~JitterBuffer() = default;

std::size_t JitterBuffer::buffered() const noexcept {
  return impl_->queue.size();
}

void JitterBuffer::push(Packet p, std::int64_t arrival_us) {
  ++stats_.pushed;
  if (static_cast<std::uint32_t>(buffered()) >= cfg_.max_buffered)
    return; // memory bound: drop rather than grow without limit

  // Jitter sample from successive arrivals (repeated timestamps allowed).
  if (stats_.pushed > 1) {
    const std::int64_t d = arrival_us - last_arrival_us_;
    jitter_window_.push_back(d < 0 ? -d : d);
    if (jitter_window_.size() > cfg_.window_size)
      jitter_window_.erase(jitter_window_.begin());
  }
  last_arrival_us_ = arrival_us;

  // Wrap-safe duplicate suppression on the stable linear position.
  if (impl_->accepted.count(p.linear_index)) {
    ++stats_.duplicates_suppressed;
    return;
  }
  impl_->accepted.insert(p.linear_index);
  impl_->queue.push({std::move(p), arrival_us});
  adapt_depth();
}

void JitterBuffer::update_bandwidth(std::uint32_t bandwidth_kbps) {
  bandwidth_kbps_ = bandwidth_kbps;
  adapt_depth();
}

void JitterBuffer::record_gap(std::uint64_t from_linear, std::uint32_t count,
                              std::int64_t now_us,
                              std::vector<Gap>* gaps) {
  ++stats_.gaps_detected;
  if (gaps) {
    Gap g;
    g.first_missing = static_cast<std::uint16_t>(from_linear);
    g.count = count;
    g.first_linear = from_linear;
    g.detected_at_us = now_us;
    gaps->push_back(g);
  }
}

void JitterBuffer::adapt_depth() {
  if (jitter_window_.empty()) {
    stats_.target_depth_us = cfg_.min_depth_us;
    return;
  }
  std::vector<std::int64_t> sorted = jitter_window_;
  std::sort(sorted.begin(), sorted.end());
  const std::size_t p95_idx = (sorted.size() * 95) / 100;
  stats_.jitter_p95_us = sorted[std::min(p95_idx, sorted.size() - 1)];

  // Low bandwidth adds serialization headroom that dominates on constrained
  // links; the whole result is always clamped to the configured hard bounds.
  const std::int64_t bw_penalty_us =
      static_cast<std::int64_t>(120'000'000 /
                                std::max<std::uint32_t>(bandwidth_kbps_, 1));
  const std::int64_t target =
      stats_.jitter_p95_us * 2 + bw_penalty_us;
  stats_.target_depth_us =
      std::max(cfg_.min_depth_us, std::min(cfg_.max_depth_us, target));
}

std::vector<Packet> JitterBuffer::drain(std::int64_t now_us,
                                        std::vector<Gap>* gaps) {
  std::vector<Packet> out;
  const std::int64_t playout_deadline = now_us - stats_.target_depth_us;

  while (!impl_->queue.empty()) {
    const Entry& top = impl_->queue.top();
    // Wait while the oldest packet is still inside the target depth window.
    if (top.arrival_us > playout_deadline) break;

    if (have_next_ && top.packet.linear_index != next_linear_) {
      const std::uint64_t want = next_linear_;
      const std::uint32_t missing = static_cast<std::uint32_t>(
          top.packet.linear_index - want);
      record_gap(want, missing, now_us, gaps);
      next_linear_ = top.packet.linear_index;
    }
    Packet emit_pkt = top.packet;
    const std::int64_t latency = now_us - top.arrival_us;
    stats_.latency_us.push_back(latency >= 0 ? latency : 0);
    impl_->queue.pop();
    have_next_ = true;
    next_linear_ = emit_pkt.linear_index + 1;
    playout_clock_us_ = now_us;
    ++stats_.emitted;
    out.push_back(std::move(emit_pkt));
  }
  return out;
}

std::vector<Packet> JitterBuffer::flush(std::vector<Gap>* gaps) {
  std::vector<Packet> out;
  while (!impl_->queue.empty()) {
    const Entry top = impl_->queue.top();
    if (have_next_ && top.packet.linear_index != next_linear_) {
      record_gap(next_linear_,
                 static_cast<std::uint32_t>(top.packet.linear_index -
                                            next_linear_),
                 top.arrival_us, gaps);
      next_linear_ = top.packet.linear_index;
    }
    impl_->queue.pop();
    have_next_ = true;
    next_linear_ = top.packet.linear_index + 1;
    ++stats_.emitted;
    out.push_back(top.packet);
  }
  impl_->accepted.clear();
  return out;
}

} // namespace media
