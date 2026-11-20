#include "relay/packet_reorder.hpp"

#include <algorithm>
#include <map>
#include <set>
#include <tuple>

namespace relay {
namespace {

struct Candidate {
  const CatalogFrame* frame = nullptr;
  std::int64_t unwrapped = 0;
  int epoch = 0;
  std::size_t arrival_order = 0;
};

/// Unwraps a 32-bit microsecond counter.  A backward jump larger than half
/// the modulus is a wrap; a smaller backward jump is genuine reordering.
struct Unwrapper {
  std::int64_t wraps = 0;
  bool primed = false;
  std::int64_t last = 0;

  std::pair<std::int64_t, bool> feed(std::uint32_t raw) {
    const std::int64_t value = raw;
    if (!primed) {
      primed = true;
      last = value;
      return {wraps * kTimestampModulus + value, false};
    }
    const std::int64_t delta = value - last;
    bool wrapped = false;
    if (delta < -kTimestampModulus / 2) {
      ++wraps;
      wrapped = true;
    } else if (delta > kTimestampModulus / 2) {
      --wraps;
    }
    last = value;
    return {wraps * kTimestampModulus + value, wrapped};
  }
};

}  // namespace

bool TrackStream::empty() const {
  return std::all_of(epochs.begin(), epochs.end(),
                     [](const TimelineEpoch& e) { return e.frames.empty(); });
}

std::size_t TrackStream::frame_count() const {
  std::size_t total = 0;
  for (const auto& epoch : epochs) {
    total += epoch.frames.size();
  }
  return total;
}

std::vector<LostRange> TrackStream::all_losses() const {
  std::vector<LostRange> out;
  for (const auto& epoch : epochs) {
    out.insert(out.end(), epoch.losses.begin(), epoch.losses.end());
  }
  return out;
}

std::optional<Micros> TrackStream::first_pts() const {
  for (const auto& epoch : epochs) {
    if (!epoch.frames.empty()) {
      return epoch.frames.front().pts_us;
    }
  }
  return std::nullopt;
}

std::optional<Micros> TrackStream::last_pts() const {
  for (auto it = epochs.rbegin(); it != epochs.rend(); ++it) {
    if (!it->frames.empty()) {
      return it->frames.back().pts_us;
    }
  }
  return std::nullopt;
}

TrackStream reorder_track(const TrackInput& input, ErrorSink& errors) {
  TrackStream stream;
  TrackSpec spec = input.spec;
  spec.policy.reorder_window = input.reorder_window;
  const TrackPolicy& policy = spec.policy;
  stream.track = input.spec.id;
  stream.timebase = input.timebase;
  stream.stats.received = input.arrivals.size();

  const double frame_us = input.timebase.frame_duration_us();

  std::vector<Candidate> candidates;
  candidates.reserve(input.arrivals.size());
  std::set<std::uint64_t> seen_sources;
  Unwrapper unwrapper;
  int epoch_index = 0;
  Micros epoch_origin = 0;
  bool have_epoch = false;
  std::uint32_t prev_seq = 0;
  bool have_prev_seq = false;
  std::int64_t prev_unwrapped = 0;

  auto close_epoch = [&](std::string cause) {
    stream.epochs.back().cause = std::move(cause);
    ++epoch_index;
    have_epoch = false;
    have_prev_seq = false;
  };

  for (const auto& arrival : input.arrivals) {
    const CatalogFrame& catalog_frame = input.catalog.at(arrival.source_frame);
    if (!catalog_frame.verified) {
      ++stream.stats.unverified_quarantined;
      continue;
    }
    const FrameRecord& record = catalog_frame.record;
    const auto [unwrapped, wrapped] = unwrapper.feed(record.raw_timestamp_us);

    bool split = false;
    std::string cause;
    if (wrapped) {
      split = true;
      cause = "timestamp wrap";
    } else if (have_prev_seq && unwrapped > prev_unwrapped &&
               record.sequence < prev_seq) {
      // A lower sequence combined with a later timestamp cannot come from
      // reordering (which only moves packets earlier in time).  The
      // capture restarted its numbering, so a new epoch begins.
      split = true;
      cause = "sequence reset";
    }
    if (split && have_epoch) {
      errors.warn("reorder", stream.track, record.source_frame,
                  "start timeline epoch " + std::to_string(epoch_index + 1) +
                      " at source frame " +
                      std::to_string(record.source_frame) + ": " + cause);
      close_epoch(cause);
    }
    if (!have_epoch) {
      epoch_origin = unwrapped;
      stream.epochs.push_back(
          TimelineEpoch{epoch_index, "start", epoch_origin, {}, {}, 0, 0, 0});
      have_epoch = true;
    }

    if (seen_sources.count(arrival.source_frame) != 0) {
      ++stream.stats.duplicates;
      ++stream.epochs.back().duplicates;
      continue;
    }
    seen_sources.insert(arrival.source_frame);
    if (have_prev_seq && record.sequence < prev_seq) {
      ++stream.stats.reordered;
    }
    prev_seq = record.sequence;
    have_prev_seq = true;
    prev_unwrapped = unwrapped;
    candidates.push_back(Candidate{&catalog_frame, unwrapped, epoch_index,
                                   candidates.size()});
  }
  if (have_epoch) {
    stream.epochs.back().cause = "open";
  }
  stream.stats.epochs = stream.epochs.size();

  const Micros window_us =
      static_cast<Micros>(frame_us * static_cast<double>(policy.reorder_window));

  // Lateness is judged in arrival order: a packet whose timestamp is more
  // than one window behind the newest timestamp already accepted can no
  // longer be placed without rewriting the output, so it is dropped.
  // Then accepted packets are sorted per epoch for delivery.
  std::stable_sort(candidates.begin(), candidates.end(),
                   [](const Candidate& a, const Candidate& b) {
                     return a.arrival_order < b.arrival_order;
                   });
  std::map<int, Micros> max_pts;
  std::map<int, std::deque<std::pair<Micros, std::size_t>>> window_bytes;
  std::map<int, std::size_t> window_bytes_open;
  std::map<int, std::vector<const Candidate*>> accepted;
  for (const auto& candidate : candidates) {
    const Micros origin =
        stream.epochs[static_cast<std::size_t>(candidate.epoch)].origin_us;
    const Micros pts = candidate.unwrapped - origin;
    auto max_it = max_pts.find(candidate.epoch);
    if (max_it != max_pts.end() && pts + window_us < max_it->second) {
      ++stream.stats.late_dropped;
      ++stream.epochs[static_cast<std::size_t>(candidate.epoch)].late_dropped;
      errors.warn("reorder", stream.track,
                  candidate.frame->record.source_frame,
                  "late packet beyond reorder window dropped");
      continue;
    }
    if (max_it == max_pts.end() || pts > max_it->second) {
      max_pts[candidate.epoch] = pts;
    }
    auto& queue = window_bytes[candidate.epoch];
    auto& open_bytes = window_bytes_open[candidate.epoch];
    const std::size_t bytes = candidate.frame->record.payload_bytes;
    queue.emplace_back(pts, bytes);
    open_bytes += bytes;
    const Micros newest = max_pts[candidate.epoch];
    while (!queue.empty() && queue.front().first + window_us < newest) {
      open_bytes -= queue.front().second;
      queue.pop_front();
    }
    stream.stats.peak_cache_bytes =
        std::max(stream.stats.peak_cache_bytes, open_bytes);
    if (open_bytes > policy.cache_limit_bytes) {
      // The bound is a hard contract: exceeding it is a data/config error,
      // never silent memory growth.
      errors.fail("reorder", stream.track,
                  candidate.frame->record.source_frame,
                  "reorder window cache exceeded " +
                      std::to_string(policy.cache_limit_bytes) + " bytes");
      break;
    }
    accepted[candidate.epoch].push_back(&candidate);
  }

  for (auto& epoch : stream.epochs) {
    auto entry = accepted.find(epoch.index);
    if (entry == accepted.end()) {
      continue;
    }
    for (const Candidate* candidate : entry->second) {
      FrameRecord frame = candidate->frame->record;
      frame.pts_us = candidate->unwrapped - epoch.origin_us;
      frame.epoch = epoch.index;
      epoch.frames.push_back(std::move(frame));
    }
    std::stable_sort(epoch.frames.begin(), epoch.frames.end(),
                     [](const FrameRecord& a, const FrameRecord& b) {
                       return std::tie(a.pts_us, a.sequence) <
                              std::tie(b.pts_us, b.sequence);
                     });
    if (!epoch.frames.empty()) {
      epoch.first_sequence = epoch.frames.front().sequence;
    }
    for (std::size_t i = 1; i < epoch.frames.size(); ++i) {
      const FrameRecord& prev = epoch.frames[i - 1];
      const FrameRecord& current = epoch.frames[i];
      if (current.sequence == prev.sequence + 1) {
        continue;
      }
      const std::uint32_t missing =
          current.sequence > prev.sequence ? current.sequence - prev.sequence - 1
                                           : 1;
      // Policy: a burst loss is one contiguous gap; single-frame policy
      // reports each missing frame separately for per-frame accounting.
      if (policy.loss_mode == LossMode::kBurst) {
        epoch.losses.push_back(LostRange{stream.track, epoch.index,
                                         prev.sequence + 1, missing,
                                         "burst loss in delivery stream"});
      } else {
        for (std::uint32_t k = 0; k < missing; ++k) {
          epoch.losses.push_back(LostRange{stream.track, epoch.index,
                                           prev.sequence + 1 + k, 1,
                                           "single-frame loss"});
        }
      }
      stream.stats.lost_marked += missing;
      errors.warn("reorder", stream.track, prev.source_frame + 1,
                  "gap of " + std::to_string(missing) +
                      " frame(s) marked per track policy");
    }
  }
  return stream;
}

}  // namespace relay
