#pragma once

#include <string>

#include "frame.hpp"

// Reads a binary P6 PPM into a 3-bytes-per-pixel FrameBuffer.
//
// Strict on purpose: only maxval 255 is accepted, dimension products are
// overflow-checked, and the raster must be exactly width*height*3 bytes
// (truncated or trailing data is an error). Throws std::runtime_error
// with a descriptive message on any violation.
FrameBuffer read_ppm(const std::string& path);

// Writes a grayscale (bpp == 1) frame as binary P6, replicating the gray
// value into the R/G/B channels. Only pixel bytes are written; row
// padding never reaches the file. Throws std::runtime_error on I/O
// failure.
void write_ppm_gray(const std::string& path, const FrameBuffer& img);
