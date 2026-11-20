#include "relay/ladder.hpp"

#include <algorithm>
#include <deque>
#include <sstream>

namespace relay {
namespace {

std::uint64_t sliding_median(const std::deque<std::uint64_t>& window) {
  if (window.empty()) {
    return 0;
  }
  std::vector<std::uint64_t> values(window.begin(), window.end());
  std::sort(values.begin(), values.end());
  return values[values.size() / 2];
}

}  // namespace

LadderResult align_ladder(const TrackSegments& boundary_source,
                          const std::vector<BandwidthSample>& samples,
                          const LadderOptions& options, ErrorSink& errors) {
  LadderResult result;
  if (boundary_source.segments.empty()) {
    errors.warn("ladder", boundary_source.track, 0,
                "no segments to align; ladder is empty");
    return result;
  }
  if (options.representations.empty()) {
    errors.fail("ladder", boundary_source.track, 0,
                "no representations requested");
    return result;
  }
  std::vector<Representation> reps = options.representations;
  std::sort(reps.begin(), reps.end(),
            [](const Representation& a, const Representation& b) {
              return a.bitrate_bps < b.bitrate_bps;
            });

  result.boundary_source = boundary_source.track;
  for (const auto& segment : boundary_source.segments) {
    result.shared_cuts_us.push_back(segment.start_us);
  }

  // Samples are consumed once, in time order, and retained only inside the
  // sliding window so memory stays bounded regardless of stream length.
  std::deque<std::uint64_t> window;
  std::size_t sample_index = 0;
  std::uint64_t estimate = 0;
  std::string current;
  std::uint64_t current_bitrate = 0;
  for (const auto& segment : boundary_source.segments) {
    while (sample_index < samples.size() &&
           samples[sample_index].at_us <= segment.start_us) {
      window.push_back(samples[sample_index].throughput_bps);
      while (window.size() > options.window_samples) {
        window.pop_front();
      }
      ++sample_index;
    }
    if (!window.empty()) {
      estimate = sliding_median(window);
    }
    // Before any sample arrives, assume the lowest rendition is safe
    // rather than optimistically upgrading.
    const auto sustainable = static_cast<std::uint64_t>(
        static_cast<double>(estimate) * options.safety);
    const Representation* chosen = &reps.front();
    for (const auto& rep : reps) {
      if (rep.bitrate_bps <= sustainable) {
        chosen = &rep;
      } else {
        break;
      }
    }
    std::string reason;
    if (estimate == 0) {
      reason = "no bandwidth sample yet; lowest rendition";
    } else {
      reason = "sliding estimate " + std::to_string(estimate) +
               " bps -> " + chosen->name;
    }

    // The cache this segment needs for the chosen rendition, capped by the
    // per-track limit.  On a drop the cap keeps the buffer bounded.
    const auto duration_us = segment.end_us - segment.start_us;
    const auto wanted = static_cast<std::size_t>(
        static_cast<double>(chosen->bitrate_bps) / 8.0 *
        static_cast<double>(duration_us) / 1'000'000.0);
    const std::size_t cache_bytes =
        std::min<std::size_t>(wanted, options.cache_limit_bytes);

    if (!current.empty() && current != chosen->name) {
      DowngradeEvent event;
      event.segment_index = segment.index;
      event.from = current;
      event.to = chosen->name;
      event.estimate_bps = estimate;
      event.cache_bytes = cache_bytes;
      event.reason = chosen->bitrate_bps < current_bitrate ? "bandwidth drop"
                                                           : "bandwidth recovery";
      errors.warn("ladder", boundary_source.track, segment.index,
                  "rendition " + event.from + " -> " + event.to + " (" +
                      event.reason + ", estimate " + std::to_string(estimate) +
                      " bps)");
      result.downgrades.push_back(std::move(event));
    }
    current = chosen->name;
    current_bitrate = chosen->bitrate_bps;

    SegmentRendition rendition;
    rendition.segment_index = segment.index;
    rendition.start_us = segment.start_us;
    rendition.end_us = segment.end_us;
    rendition.name = chosen->name;
    rendition.bitrate_bps = chosen->bitrate_bps;
    rendition.reason = reason;
    result.renditions.push_back(std::move(rendition));
    result.peak_cache_bytes =
        std::max<std::size_t>(result.peak_cache_bytes, cache_bytes);
    if (cache_bytes >= options.cache_limit_bytes && wanted > cache_bytes) {
      errors.warn("ladder", boundary_source.track, segment.index,
                  "cache clamped to " + std::to_string(options.cache_limit_bytes) +
                      " bytes for rendition " + chosen->name);
    }
  }
  return result;
}

}  // namespace relay
