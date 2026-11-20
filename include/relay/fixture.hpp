#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "relay/types.hpp"

namespace relay::fixture {

/// Options for locally generated image-frame fixtures.  Nothing is read
/// from outside the working directory.
struct GenerateOptions {
  unsigned seed = 42;
  std::filesystem::path root;
  int frames = 240;
  int gop = 60;
  int width = 96;
  int height = 54;
  std::vector<std::string> cameras{"cam-a", "cam-b", "cam-c"};
  int segment_ms = 2000;
  int loss_burst = 5;
  bool timestamp_wrap = true;
  bool sequence_reset = true;
  bool corrupt = false;
  std::size_t corrupt_camera_index = 0;
  int corrupt_frame = 62;
  bool lose_keyframe = false;
  bool empty_track = false;
  int duplicate_stride = 17;
  int reorder_stride = 11;
};

struct CameraFixture {
  std::string track;
  std::filesystem::path frames_path;
  std::filesystem::path arrival_path;
  std::size_t canonical_frames = 0;
  std::size_t arrivals = 0;
  std::vector<std::uint64_t> corrupted_frames;
};

struct GeneratedFixture {
  std::filesystem::path root;
  Timebase timebase;
  std::vector<TrackSpec> tracks;
  std::vector<CameraFixture> cameras;
  std::size_t expected_duplicates = 0;
  std::size_t expected_lost = 0;
  std::size_t expected_reordered = 0;
  std::string input_digest;
};

/// Deterministically writes `<root>/inputs/<track>.frames` (metadata +
/// checksum + PNG payload) and `<root>/inputs/<track>.arrival`
/// (arrival index, source frame id; duplicates and losses included).
GeneratedFixture generate(const GenerateOptions& options, ErrorSink& errors);

/// SHA-256 over every regular file below `root`, sorted by relative path.
std::string directory_digest(const std::filesystem::path& root);

}  // namespace relay::fixture
