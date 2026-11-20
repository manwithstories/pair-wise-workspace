#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/types.hpp"

namespace mediaqc {

// Low-level media writing shared by the sample generator and the tests.
// Both containers are written with libavformat using lossless codecs so the
// pipeline can verify payload checksums without re-encoding artefacts.
struct VideoSpec {
  int width = 64;
  int height = 48;
  Rational rate{25, 1};
  std::uint64_t frame_count = 0;
  std::uint64_t seed = 0;
};

struct AudioSpec {
  int sample_rate = 48000;
  int channels = 1;
  std::uint64_t frame_count = 0;
  std::uint64_t frames_per_packet = 1024;
  std::uint64_t seed = 0;
};

// Deterministic frame payloads.  The same seed and index always yield the same
// bytes, which is what makes generated deliveries reproducible.
[[nodiscard]] std::vector<std::uint8_t> make_video_frame(const VideoSpec& spec,
                                                         std::uint64_t index);
[[nodiscard]] std::vector<std::uint8_t> make_audio_frame(const AudioSpec& spec,
                                                         std::uint64_t index);

// Writes a full lossless container.  `out_payloads`, when non-null, receives
// the exact encoded packet bytes in order so the generator can hash them into
// the metadata stream.
[[nodiscard]] bool write_video_container(
    const std::string& path, const VideoSpec& spec,
    std::vector<std::vector<std::uint8_t>>* out_payloads, std::string& error);

[[nodiscard]] bool write_audio_container(
    const std::string& path, const AudioSpec& spec,
    std::vector<std::vector<std::uint8_t>>* out_payloads, std::string& error);

}  // namespace mediaqc
