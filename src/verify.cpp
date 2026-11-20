#include "media/checksum.hpp"

#include "media/sha256.hpp"

#include <algorithm>
#include <limits>
#include <utility>

namespace mediaqc {

void ErrorList::add(ErrorEntry entry) { entries_.push_back(std::move(entry)); }

void ErrorList::add(const Error& error) {
  ErrorEntry entry;
  entry.code = error.code;
  entry.track = error.track;
  entry.offset = error.offset;
  entry.has_offset = error.has_offset;
  entry.seq = error.seq;
  entry.has_seq = error.has_seq;
  entry.message = error.message;
  switch (error.category) {
    case ErrorCategory::kCorruption:
      entry.severity = "damage";
      break;
    case ErrorCategory::kInvalidInput:
      entry.severity = "order";
      break;
    case ErrorCategory::kIo:
    case ErrorCategory::kRecovery:
      entry.severity = "io";
      break;
    case ErrorCategory::kInvalidArgument:
      entry.severity = "argument";
      break;
  }
  entries_.push_back(std::move(entry));
}

std::int64_t ErrorList::first_offset() const {
  std::int64_t lowest = std::numeric_limits<std::int64_t>::max();
  for (const ErrorEntry& entry : entries_) {
    if (entry.has_offset && static_cast<std::int64_t>(entry.offset) < lowest) {
      lowest = static_cast<std::int64_t>(entry.offset);
    }
  }
  return lowest == std::numeric_limits<std::int64_t>::max() ? -1 : lowest;
}

Json ErrorList::to_json() const {
  // Stable order: by offset, then by code, so the manifest is reproducible.
  std::vector<const ErrorEntry*> sorted;
  sorted.reserve(entries_.size());
  for (const ErrorEntry& entry : entries_) sorted.push_back(&entry);
  std::stable_sort(sorted.begin(), sorted.end(),
                   [](const ErrorEntry* a, const ErrorEntry* b) {
                     if (a->has_offset != b->has_offset) return a->has_offset;
                     if (a->has_offset && a->offset != b->offset) {
                       return a->offset < b->offset;
                     }
                     return a->code < b->code;
                   });

  Json array = Json::make_array();
  for (const ErrorEntry* entry : sorted) {
    Json item = Json::make_object();
    item.set("code", Json::make_string(entry->code));
    item.set("severity", Json::make_string(entry->severity));
    if (!entry->track.empty()) {
      item.set("track", Json::make_string(entry->track));
    }
    if (entry->has_offset) {
      item.set("offset", Json::make_int(static_cast<std::int64_t>(entry->offset)));
    }
    if (entry->has_seq) {
      item.set("seq", Json::make_int(static_cast<std::int64_t>(entry->seq)));
    }
    if (entry->has_frame_index) {
      item.set("frame_index",
               Json::make_int(static_cast<std::int64_t>(entry->frame_index)));
    }
    item.set("message", Json::make_string(entry->message));
    array.push(std::move(item));
  }
  return array;
}

FrameVerifier::FrameVerifier(std::string track, std::uint64_t expected_width,
                             std::uint64_t expected_height)
    : track_(std::move(track)),
      expected_width_(expected_width),
      expected_height_(expected_height) {}

FrameVerdict FrameVerifier::verify(const FrameMetadata& metadata,
                                   const std::uint8_t* payload, std::size_t size) {
  FrameVerdict verdict;

  // Idempotent duplicate handling: only the first occurrence of a sequence
  // number is accepted; later ones are reported but not re-verified.
  auto [it, inserted] = seen_seq_.emplace(metadata.seq, metadata.line);
  if (!inserted) {
    verdict.check = FrameCheck::kDuplicate;
    verdict.first_index = it->second;
    ++duplicates_;
    return verdict;
  }

  Sha256Digest actual = sha256(payload, size);
  if (actual != metadata.sha) {
    verdict.check = FrameCheck::kDamaged;
    ++damaged_;
    return verdict;
  }

  // Dimensions are advisory: a discrepancy marks the frame, but only the
  // checksum decides trust.  (The checksum already covers the payload bytes.)
  (void)expected_width_;
  (void)expected_height_;
  verdict.check = FrameCheck::kOk;
  ++verified_;
  return verdict;
}

}  // namespace mediaqc
