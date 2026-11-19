// util.hpp -- small, dependency-free helpers shared by the pipeline.
#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <string_view>
#include <vector>

namespace mediaqc {

// Round-trips through JSON escaping; output is UTF-8 and ASCII-safe.
std::string json_escape(std::string_view in);

// Flattened single-line JSON with sorted object keys.  Insertion order is
// preserved for arrays; objects are emitted in insertion order, so callers
// that need determinism must insert in a stable order.
class JsonWriter {
 public:
  JsonWriter& begin_object();
  JsonWriter& end_object();
  JsonWriter& begin_array();
  JsonWriter& end_array();
  JsonWriter& key(std::string_view k);
  JsonWriter& value(std::string_view v);
  JsonWriter& value(const char* v) { return value(std::string_view(v)); }
  JsonWriter& value(int64_t v);
  JsonWriter& value(int v) { return value(static_cast<int64_t>(v)); }
  JsonWriter& value(uint64_t v);
  JsonWriter& value(bool v);
  JsonWriter& value(double v);

  std::string str() const { return out_; }

 private:
  void comma();
  std::string out_;
  std::vector<char> stack_;  // '{' or '['
  bool after_key_ = false;
  bool first_in_container_ = true;
};

// Deterministic 64-bit FNV-1a, used for cache keys and quick change checks.
uint64_t fnv1a64(std::string_view data);

bool read_file(const std::filesystem::path& path, std::string& out, int64_t max_bytes);
bool write_file_atomic(const std::filesystem::path& path, std::string_view data,
                       std::string& err);
bool path_is_within(const std::filesystem::path& child,
                    const std::filesystem::path& root);
// Resolves symlinks; returns false when the path does not exist.
bool canonicalize(const std::filesystem::path& path, std::filesystem::path& out);
int64_t file_size_bytes(const std::filesystem::path& path);

std::string iso8601_utc(int64_t epoch_seconds);
int64_t epoch_seconds_now();
std::string hex64(uint64_t v);

}  // namespace mediaqc
