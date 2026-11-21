#pragma once

#include "relay/media_clock.hpp"

#include <array>
#include <cstdint>
#include <cstdio>
#include <string>
#include <vector>

namespace relay {

inline constexpr size_t kSha256Len = 32;
using Sha256 = std::array<uint8_t, kSha256Len>;

// One frame's metadata + encoded payload as carried on the wire.
struct FrameRecord {
  uint64_t frame_seq = 0;
  int64_t node_clock = 0;
  TimeBase time_base;
  bool keyframe = false;
  std::vector<uint8_t> payload; // encoded image bytes
  Sha256 checksum{};            // sha256 over serialized fields + payload
  uint64_t record_offset = 0;   // offset of this record in the container
};

enum class ParseError {
  None = 0,
  BadMagic,
  TruncatedHeader,
  TruncatedField,
  InvalidLength,
  UnknownTimeBase,
  TruncatedTail,
};

const char* to_string(ParseError e);

struct ParseResult {
  ParseError error = ParseError::None;
  uint64_t error_offset = 0; // first error offset in the container
  std::string message;
  bool ok() const { return error == ParseError::None; }
};

Sha256 sha256(const uint8_t* data, size_t len);
Sha256 record_checksum(const FrameRecord& rec);
bool checksum_matches(const FrameRecord& rec);
std::string hex(const Sha256& digest);

// Writes the .frames container format (fixture generator, tests).
class FrameContainerWriter {
public:
  explicit FrameContainerWriter(const std::string& path);
  ~FrameContainerWriter();
  bool write_header(uint64_t declared_frames);
  bool write_record(const FrameRecord& rec); // fills checksum if zeroed
  bool write_trailer();
  bool good() const;

private:
  std::FILE* f_ = nullptr;
};

// Streaming reader: constant memory regardless of container size.
class FrameContainerReader {
public:
  explicit FrameContainerReader(const std::string& path);
  ~FrameContainerReader();

  // Opens and validates the container header.
  ParseResult open();
  // Reads the next record. Returns false at the trailer (clean EOF)
  // or on structural error (check last_error()).
  bool next(FrameRecord& out);
  const ParseResult& last_error() const { return err_; }
  uint64_t declared_frames() const { return declared_; }

private:
  std::FILE* f_ = nullptr;
  ParseResult err_;
  uint64_t declared_ = 0;
  uint64_t offset_ = 0;
  bool opened_ = false;
};

} // namespace relay
