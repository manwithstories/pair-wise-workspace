// Stream reader public interface. This batch introduces codec probing; the
// frame parser and index validation are added in the following batches.
#pragma once

#include <cstdint>
#include <vector>

#include "media/packet.hpp"

namespace media {

// Codec identification via structural heuristics and libavformat probing.
CodecType probe_codec(const std::vector<std::uint8_t>& bytes) noexcept;

} // namespace media
