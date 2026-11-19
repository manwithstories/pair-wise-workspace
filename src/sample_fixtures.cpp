// Index serialization and damaged-fixture construction.
// Kept separate from the generator core so each patch stays under 300 lines.
#include "media/sample_generator.hpp"

#include <sstream>

namespace media {

std::string serialize_index(const StreamIndex& idx) {
  std::ostringstream os;
  os << "media_index_v1\n";
  os << "codec=" << codec_name(idx.codec) << "\n";
  os << "declared_size=" << idx.declared_size << "\n";
  os << "entries=" << idx.entries.size() << "\n";
  for (const auto& e : idx.entries) {
    os << "entry " << e.frame_index << ' ' << e.offset << ' '
       << e.pts_us << "\n";
  }
  return os.str();
}

StreamIndex make_oversize_index(const SampleBundle& b) {
  StreamIndex idx = b.index;
  if (!idx.entries.empty())
    idx.entries.back().offset = b.video.size() + 4096; // past end of stream
  return idx;
}

StreamIndex make_missing_entry_index(const SampleBundle& b) {
  StreamIndex idx = b.index;
  if (idx.entries.size() > 2)
    idx.entries.erase(idx.entries.begin() + 1); // force scan rebuild
  return idx;
}

StreamIndex make_nonmonotonic_index(const SampleBundle& b) {
  StreamIndex idx = b.index;
  if (idx.entries.size() > 2)
    std::swap(idx.entries[0].pts_us, idx.entries[1].pts_us);
  return idx;
}

std::vector<std::uint8_t> make_truncated_stream(const SampleBundle& b) {
  std::vector<std::uint8_t> out = b.video;
  if (out.size() > 16)
    out.resize(out.size() - (out.size() > 32 ? 16 : 4));
  return out;
}

std::string make_oversize_declaration(const SampleBundle& b) {
  StreamIndex idx = b.index;
  idx.declared_size = kMaxDeclaredStreamBytes + 1;
  return serialize_index(idx);
}

} // namespace media
