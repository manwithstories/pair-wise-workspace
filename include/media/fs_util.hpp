#pragma once
// Path safety and atomic file replacement helpers.
#include <cstdint>
#include <string>
#include <vector>

namespace mediaqc::fs {

// Resolve symlinks lexically+on-disk and verify the path stays inside root.
// Throws std::runtime_error on escape attempts (including ".." that leaves
// the root and symlinks pointing outside it).
std::string safe_join(const std::string& root, const std::string& relative);

// Create directory (and parents) with 0755; throws on failure.
void ensure_dir(const std::string& path);

// Validate that dir exists, is a directory and is writable; returns its
// canonical path. Refuses symlinked components that do not resolve.
std::string validate_output_dir(const std::string& path);

// Write bytes to "<final>.tmp.<pid>" then atomically rename into place.
void write_atomic(const std::string& final_path, const std::string& bytes);

// Read an entire file (bounded by cap_bytes) or throw.
std::string read_file(const std::string& path, size_t cap_bytes = 1ULL << 30);

// Read an entire file in streaming chunks; callback may not retain the buffer.
using ChunkFn = void (*)(const unsigned char* data, size_t size, void* user);
void read_chunks(const std::string& path, ChunkFn fn, void* user);

struct StatInfo {
    int64_t size = 0;
    int64_t mtime_ns = 0;
    bool exists = false;
};
StatInfo stat(const std::string& path);

// Regular files in dir (non-recursive), names only, sorted ascending.
std::vector<std::string> list_files(const std::string& dir);

uint32_t crc32_ieee(const unsigned char* data, size_t size, uint32_t crc = 0);

}  // namespace mediaqc::fs
