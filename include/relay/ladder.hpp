#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "relay/segmenter.hpp"
#include "relay/types.hpp"

namespace relay {

struct Representation {
  std::string name;  ///< "360p", "720p", "1080p"
  int height = 0;
  std::uint64_t bitrate_bps = 0;
  std::size_t cache_budget_bytes = 0;
};

struct BandwidthSample {
  Micros at_us = 0;
  std::uint64_t throughput_bps = 0;
};

struct LadderOptions {
  std::vector<Representation> representations;
  /// Sliding window used to estimate available bandwidth.
  std::size_t window_samples = 5;
  /// Safety factor applied to the estimate before picking a rendition.
  double safety = 0.8;
  std::size_t cache_limit_bytes = kMaxTrackCacheBytes;
};

struct SegmentRendition {
  std::uint64_t segment_index = 0;
  Micros start_us = 0;
  Micros end_us = 0;
  std::string name;
  std::uint64_t bitrate_bps = 0;
  std::string reason;  ///< why this rendition was chosen
};

struct DowngradeEvent {
  std::uint64_t segment_index = 0;
  std::string from;
  std::string to;
  std::uint64_t estimate_bps = 0;
  std::size_t cache_bytes = 0;
  std::string reason;
};

struct LadderResult {
  std::vector<SegmentRendition> renditions;
  std::vector<DowngradeEvent> downgrades;
  std::uint64_t peak_cache_bytes = 0;
  std::vector<Micros> shared_cuts_us;
  std::string boundary_source;  ///< track whose cuts all layers share
};

/// Estimates bandwidth with a sliding window, then picks a rendition per
/// segment.  All representations share one cut list, so a segment boundary
/// never differs between layers.
LadderResult align_ladder(const TrackSegments& boundary_source,
                          const std::vector<BandwidthSample>& samples,
                          const LadderOptions& options, ErrorSink& errors);

}  // namespace relay
