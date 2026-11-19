#include "damage_builder.hpp"

#include <algorithm>
#include <cstddef>
#include <utility>

#include "byte_io.hpp"

namespace mfs {
namespace {

constexpr size_t kRecordPrefix = 8;
constexpr size_t kBodyFixedHeader = 32;

struct RecordView {
  size_t offset{0};
  uint32_t length{0};
};

std::vector<RecordView> scan_records(const std::vector<uint8_t>& bytes) {
  std::vector<RecordView> records;
  size_t pos = kFileHeaderSize;
  while (pos + kRecordPrefix <= bytes.size()) {
    if (bytes[pos] != 0x4D || bytes[pos + 1] != 0x46 ||
        bytes[pos + 2] != 0x50 || bytes[pos + 3] != 0x31) {
      break;
    }
    const uint32_t length = (uint32_t{bytes[pos + 4]} << 24) |
                            (uint32_t{bytes[pos + 5]} << 16) |
                            (uint32_t{bytes[pos + 6]} << 8) |
                            uint32_t{bytes[pos + 7]};
    if (pos + kRecordPrefix + length > bytes.size()) {
      break;
    }
    records.push_back(RecordView{pos, length});
    pos += kRecordPrefix + length;
  }
  return records;
}

void require(bool condition, const char* message) {
  if (!condition) {
    throw MediaError(ErrorType::LimitExceeded, message);
  }
}

void put_be32(std::vector<uint8_t>& bytes, size_t at, uint32_t value) {
  bytes[at] = static_cast<uint8_t>(value >> 24);
  bytes[at + 1] = static_cast<uint8_t>(value >> 16);
  bytes[at + 2] = static_cast<uint8_t>(value >> 8);
  bytes[at + 3] = static_cast<uint8_t>(value);
}

}  // namespace

DamageResult apply_damage(const std::vector<uint8_t>& source, DamageKind kind) {
  DamageResult result;
  result.bytes = source;
  std::vector<uint8_t>& bytes = result.bytes;

  if (kind == DamageKind::TruncatedHeader) {
    require(bytes.size() >= 20, "source too small for a truncated header");
    bytes.resize(20);
    return result;
  }
  if (kind == DamageKind::EmptySequence) {
    require(bytes.size() >= kFileHeaderSize, "source has no file header");
    bytes.resize(kFileHeaderSize);
    return result;
  }

  const std::vector<RecordView> records = scan_records(bytes);
  require(records.size() >= 6, "source needs at least six records");
  const RecordView& middle = records[records.size() / 2];

  switch (kind) {
    case DamageKind::ChecksumMismatch: {
      // The checksum is the last 32 bytes of the body, so the byte just before
      // it belongs to the payload.
      const size_t payload_byte =
          middle.offset + kRecordPrefix + middle.length - kSha256Size - 1;
      bytes[payload_byte] ^= 0xFF;
      result.offset = middle.offset;
      break;
    }
    case DamageKind::LengthOverflow: {
      const RecordView& target = records[2];
      put_be32(bytes, target.offset + 4, 0xFFFFFFF0u);
      result.offset = target.offset;
      break;
    }
    case DamageKind::TruncatedTail: {
      const RecordView& target = records[records.size() - 3];
      bytes.resize(target.offset + 16);
      result.offset = target.offset;
      break;
    }
    case DamageKind::BadMagic: {
      const RecordView& target = records[1];
      bytes[target.offset] = 'X';
      bytes[target.offset + 1] = 'X';
      bytes[target.offset + 2] = 'X';
      bytes[target.offset + 3] = 'X';
      result.offset = target.offset;
      break;
    }
    case DamageKind::UnknownRequired: {
      // Insert one extra field entry after the fixed body header and update
      // both the field count and the record length.
      const size_t count_at = middle.offset + kRecordPrefix + 22;
      require(count_at < bytes.size(), "record too small for a field table");
      const uint8_t count = bytes[count_at];
      std::vector<uint8_t> field;
      put_u16(field, 0x7FFF);
      put_u8(field, static_cast<uint8_t>(FieldKind::Required));
      put_u8(field, 0);
      put_u32(field, 1);
      put_u8(field, 0);
      const size_t insert_at = middle.offset + kRecordPrefix + kBodyFixedHeader;
      bytes.insert(bytes.begin() + static_cast<ptrdiff_t>(insert_at),
                   field.begin(), field.end());
      bytes[count_at] = static_cast<uint8_t>(count + 1);
      put_be32(bytes, middle.offset + 4, middle.length + field.size());
      result.offset = middle.offset;
      break;
    }
    case DamageKind::TruncatedHeader:
    case DamageKind::EmptySequence:
      break;
  }
  return result;
}

}  // namespace mfs
