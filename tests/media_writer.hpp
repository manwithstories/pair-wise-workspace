#pragma once

#include <cstdint>
#include <cstdio>
#include <string>
#include <vector>

#include "media/types.hpp"

namespace mediaqc::testing {

// Builds the media payloads for generated deliveries: a lossless FFV1 video
// container and a PCM audio container, both matching what the pipeline's
// decoder expects.  These are compiled into the test/generator target so no
// external fixture files are ever referenced.
class MediaWriter {
 public:
  virtual ~MediaWriter() = default;
  // Opens `path`; returns false and sets `error` on failure.
  virtual bool open(const std::string& path, std::string& error) = 0;
  virtual bool write_frame(const std::uint8_t* data, std::size_t size,
                           bool keyframe, std::string& error) = 0;
  virtual bool close(std::string& error) = 0;
};

// Encodes deterministic pattern frames with FFV1 so the output bytes are
// stable across runs on the same FFmpeg build.
[[nodiscard]] bool write_video_container(const std::string& path, int width,
                                         int height, Rational rate,
                                         std::vector<std::vector<std::uint8_t>>& frames_out,
                                         std::string& error);

// Writes a PCM s16le audio container carrying a deterministic tone.
[[nodiscard]] bool write_audio_container(const std::string& path,
                                         int sample_rate, int channels,
                                         std::vector<std::vector<std::uint8_t>>& frames_out,
                                         std::string& error);

}  // namespace mediaqc::testing
