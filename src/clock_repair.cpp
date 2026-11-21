#include "relay/media_clock.hpp"

// Skeleton: implementation lands in a follow-up commit.
namespace relay {

const char* to_string(ClockError) { return "unimplemented"; }

ClockRepairer::ClockRepairer(ClockRepairConfig cfg) : cfg_(cfg) {}

RepairedClock ClockRepairer::observe(int64_t node_clock, const TimeBase&) {
  RepairedClock r;
  r.raw = node_clock;
  r.unwrapped = node_clock;
  r.repaired = node_clock;
  return r;
}

} // namespace relay
