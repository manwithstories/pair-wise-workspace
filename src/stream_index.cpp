#include "media/stream_index.hpp"

#include <algorithm>
#include <limits>

namespace media {

void validate_index(ReadResult& r, const std::vector<std::uint8_t>& bytes,
                    const StreamIndex* supplied) {
  StreamIndex scanned;
  scanned.codec = r.validated_index.codec;
  scanned.declared_size = bytes.size();
  for (const auto& f : r.frames)
    if (f.keyframe)
      scanned.entries.push_back({f.index, f.offset, f.pts_us});

  if (!supplied) { r.validated_index = scanned; return; }
  r.validated_index = *supplied;
  if (supplied->declared_size > kMaxDeclaredStreamBytes)
    r.errors.push_back({ErrorCode::OversizeDeclaration, 0, 0,
                        "declared_size exceeds safety limit"});

  std::int64_t last_pts = std::numeric_limits<std::int64_t>::min();
  for (const auto& e : supplied->entries) {
    if (e.offset >= bytes.size()) {
      r.errors.push_back({ErrorCode::IndexOffsetOutOfRange, 0, e.frame_index,
                          "offset " + std::to_string(e.offset)});
    }
    if (e.pts_us <= last_pts) {
      r.errors.push_back({ErrorCode::IndexNotMonotonic, 0, e.frame_index,
                          "pts " + std::to_string(e.pts_us)});
    }
    last_pts = e.pts_us;
  }

  // Missing entries: scan the stream and rebuild candidate key frames.
  if (supplied->entries.size() < scanned.entries.size()) {
    for (const auto& want : scanned.entries) {
      bool have = std::any_of(supplied->entries.begin(), supplied->entries.end(),
          [&](const IndexEntry& e) { return e.frame_index == want.frame_index; });
      if (!have)
        r.errors.push_back({ErrorCode::IndexMissingEntry, 0, want.frame_index,
                            "rebuilt from stream scan at offset " +
                            std::to_string(want.offset)});
    }
    r.validated_index.entries = scanned.entries;
  }
}

} // namespace media
