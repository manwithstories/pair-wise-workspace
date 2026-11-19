#include "packet_reader.hpp"

#include <algorithm>
#include <string>
#include <utility>

#include "metadata_codec.hpp"

namespace mfs {
namespace {

constexpr size_t kRecordPrefixSize = 8;
// Fixed header (32) + payload length (4) + trailing SHA-256 (32).
constexpr size_t kMinBodySize = 32 + 4 + kSha256Size;

bool has_magic(std::span<const uint8_t> data, size_t at) {
  return at + 4 <= data.size() && data[at] == 0x4D && data[at + 1] == 0x46 &&
         data[at + 2] == 0x50 && data[at + 3] == 0x31;
}

}  // namespace

PacketReader::PacketReader(std::span<const uint8_t> data, ParseOptions options)
    : data_(data), options_(options) {
  header_ok_ = parse_header();
}

bool PacketReader::parse_header() {
  if (data_.size() < kFileHeaderSize) {
    truncated_ = true;
    report(ErrorType::FileHeaderTruncated, 0, "file header truncated");
    return false;
  }
  ByteReader reader(data_.subspan(0, kFileHeaderSize));
  if (reader.u32() != kFileMagic) {
    stop_ = true;
    report(ErrorType::FileHeaderInvalid, 0, "bad file magic");
    return false;
  }
  header_.version = reader.u16();
  reader.u16();
  header_.flags = reader.u32();
  header_.stream_id = reader.u64();
  header_.nominal_hz = reader.u32();
  reader.u32();
  reader.u32();
  position_ = kFileHeaderSize;
  if (header_.version != kFormatVersion) {
    stop_ = true;
    report(ErrorType::UnsupportedFileVersion, 4,
           "unsupported file version " + std::to_string(header_.version));
    return false;
  }
  if (header_.nominal_hz == 0) {
    header_.nominal_hz = 1000000;
  }
  return true;
}

void PacketReader::report(ErrorType type, uint64_t offset, std::string detail) {
  errors_.push_back(RepairError{type, offset, current_index_, std::move(detail)});
}

bool PacketReader::next(Packet& out) {
  return header_ok_ && !stop_ && read_record(out);
}

// Moves position_ to the next record magic at or after `from`; false at EOF.
bool PacketReader::resync(size_t from) {
  for (size_t i = from; i + 4 <= data_.size(); ++i) {
    if (has_magic(data_, i)) {
      position_ = i;
      return true;
    }
  }
  position_ = data_.size();
  return false;
}

bool PacketReader::read_record(Packet& out) {
  while (position_ + kRecordPrefixSize <= data_.size()) {
    const uint64_t offset = position_;
    ByteReader reader(data_.subspan(position_));
    current_index_ = static_cast<uint32_t>(records_seen_);
    if (reader.u32() != kPacketMagic) {
      report(ErrorType::RecordMagicMismatch, offset,
             "record magic not found; resynchronising");
      if (!resync(position_ + 1)) return false;
      continue;
    }
    const uint32_t length = reader.u32();
    const size_t body_start = position_ + kRecordPrefixSize;
    const size_t limit =
        std::min(options_.max_frame_bytes + 4096, options_.max_total_bytes);
    if (length < kMinBodySize || length > limit) {
      // An absurd length is length corruption rather than truncation, even
      // when it also happens to run past EOF.
      report(ErrorType::LengthOverflow, offset,
             "declared length " + std::to_string(length) + " outside bounds");
      if (!resync(body_start)) return false;
      continue;
    }
    if (length > data_.size() - body_start) {
      // Declared body runs past EOF. Report and resynchronise so packets after
      // the damaged record stay reachable.
      truncated_ = true;
      report(ErrorType::RecordTruncated, offset,
             "declared length " + std::to_string(length) + " exceeds " +
                 std::to_string(data_.size() - body_start) + " available");
      if (!resync(body_start)) return false;
      continue;
    }

    Packet packet;
    packet.raw_offset = offset;
    packet.record_length = length;
    position_ = body_start + length;
    current_index_ = static_cast<uint32_t>(records_seen_++);
    const BodyResult body =
        decode_packet_body(data_.subspan(body_start, length),
                           options_.max_frame_bytes, packet);
    if (body.status != BodyStatus::Ok) {
      report(body.error_type, offset, body.detail);
      continue;
    }
    body_bytes_ += length;
    if (body_bytes_ > options_.max_total_bytes) {
      stop_ = true;
      report(ErrorType::LimitExceeded, offset,
             "cumulative packet bytes exceed " +
                 std::to_string(options_.max_total_bytes));
      return false;
    }
    out = std::move(packet);
    return true;
  }

  if (position_ < data_.size()) {
    truncated_ = true;
    report(ErrorType::RecordTruncated, position_,
           "trailing bytes cannot form a record");
    position_ = data_.size();
  }
  return false;
}

}  // namespace mfs
