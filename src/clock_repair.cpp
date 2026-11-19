#include "media/clock_repair.hpp"

namespace media {
ClockRepair::ClockRepair(Rational tb, MediaType type) : tb_(tb), type_(type) {}

TimeError ClockRepair::accept(MediaPacket&) {
  // 完整实现随第三批提交。
  return TimeError::None;
}
void ClockRepair::addAnchor(double, double) {}
DriftParams ClockRepair::estimateDrift() { return summary_.drift; }
void applyDrift(std::vector<MediaPacket>&, const DriftParams&) {}
} // namespace media
