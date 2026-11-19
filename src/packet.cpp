#include "media/packet.hpp"

namespace media {
const char* toString(MediaType t) {
  return t == MediaType::Video ? "video" : "audio";
}
const char* toString(BoundaryKind k) {
  switch (k) {
  case BoundaryKind::Exact: return "exact";
  case BoundaryKind::Snapped: return "snapped";
  case BoundaryKind::MissingFrame: return "missing_frame";
  }
  return "unknown";
}
} // namespace media
