#pragma once

#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <string>
#include <string_view>
#include <vector>

#include "error.hpp"

namespace media_restore {

// Hard ceilings that keep a hostile or damaged archive from exhausting memory.
// They are deliberately generous for real reels but small enough to test.
struct Limits {
  int64_t max_media_bytes = 128LL * 1024 * 1024;
  size_t max_index_entries = 200000;
  size_t max_tracks = 64;
  size_t max_segments_per_track = 4096;
  size_t max_manifest_bytes = 16 * 1024 * 1024;
  int64_t max_audio_samples = 48000LL * 60 * 30;  // 30 min at 48 kHz
  int64_t max_audio_bytes = 256LL * 1024 * 1024;
};

// Reads a whole file after checking its size against `max_bytes`.  Never
// silently truncates: an oversized file yields E_LIMIT_EXCEEDED.
bool read_file(const std::filesystem::path& path, size_t max_bytes,
               std::string* out, Diagnostic* error);

// Writes via a sibling temporary file and an atomic rename, so a failed run
// never truncates a previously valid result.
bool write_file_atomic(const std::filesystem::path& path, std::string_view data,
                       Diagnostic* error);

bool write_text_file(const std::filesystem::path& path, std::string_view data,
                     Diagnostic* error);

// Directory creation for output trees.
bool ensure_directory(const std::filesystem::path& path, Diagnostic* error);

// Stable content hash of a file, streamed in bounded chunks.  `hash_file`
// returns the 16-hex FNV digest used for cheap identity checks;
// `sha256_file` returns the 64-hex digest used for segment payload hashes.
bool hash_file(const std::filesystem::path& path, std::string* hex,
               Diagnostic* error);
bool sha256_file(const std::filesystem::path& path, std::string* hex,
                 Diagnostic* error);

bool file_size(const std::filesystem::path& path, int64_t* size,
               Diagnostic* error);

// Resolves `relative` against the directory of `manifest_path`.  Absolute
// paths are rejected so a manifest cannot reach outside its archive.
bool resolve_archive_path(const std::filesystem::path& manifest_path,
                          std::string_view relative,
                          std::filesystem::path* out,
                          Diagnostic* error);

// Reads a raw byte range for header sniffing, clipped to the file end.
bool read_prefix(const std::filesystem::path& path, int64_t offset,
                 size_t length, std::string* out, Diagnostic* error);

}  // namespace media_restore
