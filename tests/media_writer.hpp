#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "media/fixture_generator.hpp"
#include "media/types.hpp"

namespace mediaqc::testing {

// Test-side convenience wrapper over the library container writers.  Keeps
// test cases short and makes the deterministic spec explicit at the call
// site.  Production code calls the writers in media/fixture_generator.hpp
// directly.
struct VideoContainer {
  VideoSpec spec;
  std::vector<std::vector<std::uint8_t>> packets;
};

struct AudioContainer {
  AudioSpec spec;
  std::vector<std::vector<std::uint8_t>> packets;
};

[[nodiscard]] inline bool write_video(const std::string& path, VideoContainer& out,
                                      std::string& error) {
  out.packets.clear();
  return write_video_container(path, out.spec, &out.packets, error);
}

[[nodiscard]] inline bool write_audio(const std::string& path, AudioContainer& out,
                                      std::string& error) {
  out.packets.clear();
  return write_audio_container(path, out.spec, &out.packets, error);
}

}  // namespace mediaqc::testing
