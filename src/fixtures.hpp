// fixtures.hpp -- self-generated boundary inputs.
//
// Every sample is produced locally: FFmpeg's lavfi sources encode a small
// MP4, then mediaqc patches bytes (or truncates) to create the failure cases.
// No external media is downloaded or referenced.
#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

namespace mediaqc {

enum class FixtureKind {
  Clean,
  MoovAtEnd,
  TruncatedTail,
  TruncatedMoov,
  NoMoov,
  CorruptPacket,
  IndexOutOfRange,
  IncompleteIndex,
  TimestampWrap,
  BackwardsTimestamp,
  ZeroLength,
  StaleCheckpoint,
};

const char* fixture_kind_name(FixtureKind k);

struct FixtureInfo {
  std::string name;        // file name inside the input directory
  std::string path;        // absolute path
  FixtureKind kind = FixtureKind::Clean;
  bool expect_ok = true;   // pipeline should still produce a verified output
  int64_t damage_offset = -1;  // where the fixture was damaged (if applicable)
  int64_t damage_size = 0;
  int64_t input_size = 0;
  std::string description;
};

struct FixtureSet {
  std::filesystem::path work_dir;
  std::filesystem::path input_dir;
  std::filesystem::path output_dir;
  std::filesystem::path checkpoint_dir;
  std::vector<FixtureInfo> files;
  bool ok = true;
  std::string error;
};

// Encodes the shared source clip and writes every failure variant.  The
// checkpoint directory is seeded with a deliberately stale checkpoint for the
// `stale_checkpoint.mp4` input so a resume attempt must be refused.
FixtureSet generate_fixtures(const std::filesystem::path& work_dir,
                             const std::string& ffmpeg_binary = "ffmpeg");

// Encodes a source clip at `path` (video + audio, faststart optional).
bool encode_source(const std::string& ffmpeg_binary, const std::string& path,
                   int seconds, bool faststart, std::string& err);

// Byte-level helpers, exposed for tests.
bool truncate_file(const std::filesystem::path& path, int64_t keep_bytes,
                   std::string& err);
bool drop_moov(const std::filesystem::path& src,
               const std::filesystem::path& dst, std::string& err);
bool patch_stco_entry(const std::filesystem::path& path, int64_t index,
                      int64_t new_target, std::string& err);
bool patch_stco_entry_count(const std::filesystem::path& path, int64_t new_count,
                            std::string& err);
bool corrupt_sample(const std::filesystem::path& path, int64_t sample_index,
                    int stream_ordinal, int64_t& damage_offset, int64_t& damage_size,
                    std::string& err);
bool patch_fragment_tfdt(const std::filesystem::path& path,
                         const std::vector<int64_t>& per_tfdt_values, std::string& err);
int64_t count_top_level_box(const std::filesystem::path& path, const std::string& type);

}  // namespace mediaqc
