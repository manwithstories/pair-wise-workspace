#pragma once

#include <cstdint>
#include <string>
#include <utility>

namespace media_archive {

enum class Status {
  Ok = 0,
  UsageError = 2,   // bad arguments / limits exceeded
  MediaCorrupt = 3,// stream or index too damaged to recover
  IoError = 4      // checkpoint or local write failure
};

// Human readable, locatable diagnostic. offset < 0 means no byte position.
struct Diagnostic {
  Status status = Status::Ok;
  std::string message;
  int64_t offset = -1;
  uint64_t block = UINT64_MAX; // checkpoint block number when relevant

  bool ok() const { return status == Status::Ok; }
  explicit operator bool() const { return ok(); }
};

inline Diagnostic diag(Status s, std::string msg, int64_t off = -1) {
  return Diagnostic{s, std::move(msg), off, UINT64_MAX};
}

}  // namespace media_archive
