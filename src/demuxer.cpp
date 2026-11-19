#include "media/demuxer.hpp"

namespace media {
bool loadSidecar(const std::string&, Sidecar&, std::string& err) {
  err = "sidecar loader not implemented";
  return false;
}
DemuxResult demuxTrack(const std::string&, const Sidecar&, int) {
  return DemuxResult{};
}
} // namespace media
