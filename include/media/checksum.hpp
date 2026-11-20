#pragma once

#include <cstdint>
#include <string>
#include <unordered_map>
#include <vector>

#include "media/delivery.hpp"
#include "media/json.hpp"
#include "media/types.hpp"

namespace mediaqc {

// One machine-readable problem found while validating the delivery.  Every
// entry can be located in the input: `offset` is the byte position of the
// offending frame record (JSONL line start or media packet start).
struct ErrorEntry {
  std::string code;      // stable slug: checksum_mismatch, duplicate_seq, ...
  std::string track;
  std::uint64_t offset = 0;
  bool has_offset = false;
  std::uint64_t seq = 0;
  bool has_seq = false;
  std::uint64_t frame_index = 0;
  bool has_frame_index = false;
  std::string message;
  // damage = the bytes themselves are bad; order = sequencing problem;
  // truncation = the record is incomplete.
  std::string severity = "damage";
};

// Accumulates error entries and serializes them as a stable JSON list.
class ErrorList {
 public:
  void add(ErrorEntry entry);
  void add(const Error& error);

  [[nodiscard]] const std::vector<ErrorEntry>& entries() const { return entries_; }
  [[nodiscard]] bool empty() const { return entries_.empty(); }
  [[nodiscard]] std::size_t size() const { return entries_.size(); }

  [[nodiscard]] std::int64_t first_offset() const;
  [[nodiscard]] Json to_json() const;

 private:
  std::vector<ErrorEntry> entries_;
};

// The outcome of checking one frame's payload against its recorded checksum.
enum class FrameCheck { kOk, kDamaged, kDuplicate };

struct FrameVerdict {
  FrameCheck check = FrameCheck::kOk;
  // For duplicates: the first frame index that used this sequence number.
  std::uint64_t first_index = 0;
};

// Verifies per-frame checksums and sequences for a single track.  Duplicate
// sequence numbers are handled idempotently: the first occurrence is kept and
// later ones are reported, never silently dropped from the manifest.
class FrameVerifier {
 public:
  FrameVerifier(std::string track, std::uint64_t expected_width,
                std::uint64_t expected_height);

  // `payload` is the bytes read from the media container.  `metadata` carries
  // the supplier checksum and sequence.
  [[nodiscard]] FrameVerdict verify(const FrameMetadata& metadata,
                                    const std::uint8_t* payload,
                                    std::size_t size);

  [[nodiscard]] std::uint64_t verified() const { return verified_; }
  [[nodiscard]] std::uint64_t damaged() const { return damaged_; }
  [[nodiscard]] std::uint64_t duplicates() const { return duplicates_; }

 private:
  std::string track_;
  std::uint64_t expected_width_ = 0;
  std::uint64_t expected_height_ = 0;
  std::unordered_map<std::uint64_t, std::uint64_t> seen_seq_;
  std::uint64_t verified_ = 0;
  std::uint64_t damaged_ = 0;
  std::uint64_t duplicates_ = 0;
};

}  // namespace mediaqc
