#pragma once

// Damaged-sample builder.
//
// The acceptance fixtures are derived from a generated clip so they always
// describe real byte offsets. Building them in-tree keeps the demo to one
// toolchain (`brew install cmake ffmpeg catch2`) and avoids any scripting
// runtime besides the shell driver.

#include <cstdint>
#include <string>
#include <vector>

#include "media_types.hpp"

namespace mfs {

enum class DamageKind {
  ChecksumMismatch,  // flip one payload byte of a middle record
  LengthOverflow,    // overwrite a record's length with an absurd value
  TruncatedHeader,   // cut the file inside the 32-byte header
  TruncatedTail,     // cut the file inside one of the last records
  EmptySequence,     // keep only the file header
  BadMagic,          // corrupt the magic of an early record
  UnknownRequired,   // inject an unknown required metadata field
};

struct DamageResult {
  std::vector<uint8_t> bytes;
  // Byte offset of the damaged record (0 for header-level damage).
  uint64_t offset{0};
};

// Applies `kind` to a well-formed MFS1 buffer. Throws MediaError when the
// source does not contain enough records for the requested damage.
DamageResult apply_damage(const std::vector<uint8_t>& source, DamageKind kind);

}  // namespace mfs
