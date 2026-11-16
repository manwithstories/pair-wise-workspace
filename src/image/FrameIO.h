#pragma once
#include <string>
#include <vector>

namespace lg {

// Interleaved RGB float frame, nominal range [0,1].
struct Frame {
  int w = 0;
  int h = 0;
  std::vector<float> px;
};

// Reads .ppm (P6/P3), .pam (P7) and .ff (raw float32) by magic bytes.
// Throws IoError on missing/malformed files, InternalError on NaN pixels.
Frame readFrame(const std::string& path);

// Writes .ppm (P6/8-bit), .pam (16-bit) or .ff (float32) by extension.
void writeFrame(const Frame& f, const std::string& path);

bool isFrameFile(const std::string& path);

// Deterministic synthetic gradient used by demo fixtures.
Frame makeGradient(int w, int h, double phase);

} // namespace lg
