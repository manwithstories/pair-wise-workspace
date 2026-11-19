#include "media/stream_reader.hpp"

#include <cstring>
#include <sstream>

namespace media {

namespace {

bool starts_with(const std::string& s, const char* p) {
  const std::size_t n = std::char_traits<char>::length(p);
  return s.size() >= n && s.compare(0, n, p) == 0;
}

} // namespace

bool parse_index_text(const std::string& text, StreamIndex& out,
                      std::string& error) {
  std::istringstream is(text);
  std::string line;
  if (!std::getline(is, line) || line != "media_index_v1") {
    error = "missing media_index_v1 header";
    return false;
  }
  out = StreamIndex{};
  std::uint32_t declared_entries = 0;
  bool have_count = false;
  while (std::getline(is, line)) {
    if (line.empty()) continue;
    if (starts_with(line, "codec=")) {
      out.codec = codec_from_string(line.substr(6));
    } else if (starts_with(line, "declared_size=")) {
      out.declared_size = std::stoull(line.substr(14));
    } else if (starts_with(line, "entries=")) {
      declared_entries = static_cast<std::uint32_t>(
          std::stoul(line.substr(8)));
      have_count = true;
    } else if (starts_with(line, "entry ")) {
      std::istringstream es(line.substr(6));
      IndexEntry e;
      if (!(es >> e.frame_index >> e.offset >> e.pts_us)) {
        error = "malformed entry: " + line;
        return false;
      }
      out.entries.push_back(e);
    } else {
      error = "unknown index line: " + line;
      return false;
    }
  }
  if (!have_count || out.entries.size() != declared_entries) {
    error = "entry count mismatch";
    return false;
  }
  return true;
}

} // namespace media
