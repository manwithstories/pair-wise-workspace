#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/types.hpp"

namespace mediaqc {

// Validates the track dependency graph and produces the single processing
// order the pipeline uses.  The order is deterministic: among tracks that are
// ready at the same time, declaration order wins, so identical input always
// yields identical output bytes and identical error reports.
struct TrackOrder {
  std::vector<std::size_t> order;      // track indices in processing order
  std::vector<std::string> report;     // human-readable ordering notes
};

struct OrderError {
  Error error;
  std::string track;                   // first offending track
  std::vector<std::string> cycle;      // tracks on the offending cycle
};

class TrackGraph {
 public:
  // `tracks` must be the full declaration list.  Duplicate ids, unknown
  // dependencies and cycles are rejected with the first offending track.
  static bool build(const std::vector<TrackDesc>& tracks, TrackOrder& out,
                    OrderError& error);
};

}  // namespace mediaqc
