#include "media/manifest.hpp"
#include <charconv>
#include <sstream>

namespace media {
const TrackSpec* Manifest::findTrack(const std::string& id) const {
  for (const auto& t : tracks)
    if (t.id == id) return &t;
  return nullptr;
}

// 完整实现随第二批提交；骨架阶段安全返回错误。
ManifestError parseManifest(const std::string&, Manifest&) {
  return ManifestError{0, "manifest parser not implemented"};
}
std::string manifestToText(const Manifest&) { return {}; }

BoundaryState alignEditPoint(const EditPoint& edit,
                             const std::vector<MediaPacket>& packets) {
  BoundaryState st;
  st.editIndex = edit.index;
  st.targetUs = edit.targetUs;
  if (packets.empty()) {
    st.kind = BoundaryKind::MissingFrame;
    return st;
  }
  st.actualUs = packets.front().repairedUs;
  st.errorUs = st.actualUs - st.targetUs;
  return st;
}
} // namespace media
