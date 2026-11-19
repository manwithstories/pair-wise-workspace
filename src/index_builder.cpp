#include "media/index_builder.hpp"

namespace media {
void KeyframeIndex::build(const std::vector<MediaPacket>&) {}

std::optional<SeekResult> KeyframeIndex::seek(int64_t targetUs) const {
  if (entries_.empty()) return std::nullopt;
  SeekResult r;
  r.targetUs = targetUs;
  r.entry = entries_.front();
  r.errorUs = r.entry.ptsUs - targetUs;
  return r;
}

TimeError KeyframeIndex::writeAtomic(const std::string&) const {
  return TimeError::IoError; // 完整实现随第六批提交
}
bool KeyframeIndex::isComplete(const std::string&) { return false; }
} // namespace media
