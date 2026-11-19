#pragma once

// Sequential parser for the MFS1 capture container.
//
// Framing errors (bad magic, truncated or absurd length) are recovered by
// resynchronising on the next record magic, so packets after a damaged record
// stay reachable. Every recoverable problem is reported through errors() with
// the byte offset of the offending record.

#include <cstdint>
#include <span>
#include <string>
#include <vector>

#include "byte_io.hpp"
#include "media_types.hpp"

namespace mfs {

struct ParseOptions {
  size_t max_frame_bytes{kDefaultMaxFrameBytes};
  size_t max_total_bytes{kDefaultMaxTotalBytes};
};

class PacketReader {
 public:
  PacketReader(std::span<const uint8_t> data, ParseOptions options = {});

  // Yields the next valid packet; returns false at EOF or after a fatal
  // structural error (bad file header, cumulative memory limit).
  bool next(Packet& out);

  const FileHeader& header() const { return header_; }
  const std::vector<RepairError>& errors() const { return errors_; }
  bool truncated() const { return truncated_; }
  uint64_t records_seen() const { return records_seen_; }
  uint64_t body_bytes() const { return body_bytes_; }

 private:
  bool parse_header();
  bool read_record(Packet& out);
  bool resync(size_t from);
  void report(ErrorType type, uint64_t offset, std::string detail);

  std::span<const uint8_t> data_;
  ParseOptions options_;
  FileHeader header_;
  std::vector<RepairError> errors_;
  size_t position_{0};
  uint64_t records_seen_{0};
  uint64_t body_bytes_{0};
  uint32_t current_index_{0};
  bool header_ok_{false};
  bool truncated_{false};
  bool stop_{false};
};

}  // namespace mfs
