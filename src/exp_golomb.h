#pragma once

#include "bitreader.h"

#include <cstdint>

namespace h264 {

std::uint32_t readUe(BitReader& reader);
std::int32_t readSe(BitReader& reader);
std::uint32_t readTe(BitReader& reader, std::uint32_t range);

}  // namespace h264
