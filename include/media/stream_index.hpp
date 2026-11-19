// Internal helper: validate a supplied key frame index against parsed frames.
#pragma once

#include <cstdint>
#include <vector>

#include "media/packet.hpp"
#include "media/stream_reader.hpp"

namespace media {

void validate_index(ReadResult& r, const std::vector<std::uint8_t>& bytes,
                    const StreamIndex* supplied);

} // namespace media
