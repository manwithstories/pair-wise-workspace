#pragma once

// FFmpeg-backed frame codec.
//
// Decoding validates that a payload is a real image of the declared size and
// produces a canonical RGB24 view whose digest becomes the frame hash. A
// private, single-threaded decoder context is created per call, so one damaged
// frame can never poison the next.
//
// Encoding is used by the generator to synthesise the sample clip; it emits
// PNG payloads deterministically (fixed compression level, no timestamps).

#include <cstdint>
#include <span>
#include <string>
#include <vector>

#include "media_types.hpp"
#include "sha256.hpp"

namespace mfs {

struct DecodeOutcome {
  bool ok{false};
  uint32_t width{0};
  uint32_t height{0};
  Sha256Digest content_sha256{};
  std::string detail;
};

class FrameTransform {
 public:
  FrameTransform();
  ~FrameTransform();

  FrameTransform(const FrameTransform&) = delete;
  FrameTransform& operator=(const FrameTransform&) = delete;

  // Decodes `payload` and verifies it against the declared geometry. Never
  // throws for bad image data: failures are reported through DecodeOutcome.
  DecodeOutcome decode(CodecId codec, std::span<const uint8_t> payload,
                       uint32_t expected_width, uint32_t expected_height);

  // Encodes RGB24 pixels into a PNG payload. Returns false on failure.
  bool encode_png(std::span<const uint8_t> rgb24, uint32_t width,
                  uint32_t height, std::vector<uint8_t>& out);

 private:
  struct Impl;
  Impl* impl_;
};

// Diagnostic used by the pipeline: the FFmpeg error string for a return code.
std::string ffmpeg_error_string(int errnum);

}  // namespace mfs
