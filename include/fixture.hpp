#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "error.hpp"
#include "manifest.hpp"

namespace media_restore {

struct FixtureOptions {
  std::filesystem::path out_dir;
  bool include_damaged = false;
};

struct GeneratedKeyframe {
  int64_t tick = 0;    // in the manifest time base
  int64_t offset = 0;  // byte offset in the raw stream
  int64_t length = 0;
};

// Encodes a deterministic test pattern to a raw H.264, H.265 or AV1 stream.
// Keyframe positions come from the encoder packets themselves, so generated
// indexes are exact rather than inferred.
bool encode_raw_fixture(Codec codec, const std::filesystem::path& path,
                        int frames, int gop, int width, int height,
                        std::vector<GeneratedKeyframe>* keyframes,
                        Diagnostic* error);

// Writes a deterministic WAV fixture with the given tone parameters.
bool write_audio_fixture(const std::filesystem::path& path, int sample_rate,
                         const std::string& layout, int64_t frames,
                         double hz, double amplitude, bool silence,
                         Diagnostic* error);

// Generates the whole sample archive (manifest, raw streams, keyframe index
// files, audio media and, optionally, the damaged variants).
bool generate_archive(const FixtureOptions& options,
                      std::vector<Diagnostic>* diagnostics);

// Raw stream file extension for a codec ("h264", "h265", "obu").
const char* extension_for(Codec codec);

}  // namespace media_restore
