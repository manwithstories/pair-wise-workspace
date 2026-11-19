#include "metadata_codec.hpp"

#include <algorithm>
#include <string>

#include "byte_io.hpp"
#include "sha256.hpp"

namespace mfs {
namespace {

constexpr size_t kFieldHeaderSize = 8;

bool known_field(uint16_t id) {
  return id == kFieldCameraId || id == kFieldLinkType ||
         id == kFieldCaptureNote || id == kFieldArrivalUs;
}

BodyResult fail(BodyStatus status, ErrorType type, std::string detail) {
  return BodyResult{status, type, std::move(detail)};
}

// Reads the metadata field table. Unknown optional fields are preserved; an
// unknown required field rejects the packet because it cannot be interpreted.
BodyResult read_fields(ByteReader& reader, uint8_t count, PacketHeader& h) {
  for (uint8_t i = 0; i < count; ++i) {
    if (!reader.has(kFieldHeaderSize)) {
      return fail(BodyStatus::Malformed, ErrorType::UnsupportedMetadata,
                  "metadata field header truncated");
    }
    const uint16_t id = reader.u16();
    const auto kind = static_cast<FieldKind>(reader.u8());
    reader.u8();
    const uint32_t value_len = reader.u32();
    if (kind != FieldKind::Required && kind != FieldKind::Optional) {
      return fail(BodyStatus::Malformed, ErrorType::UnsupportedMetadata,
                  "invalid metadata field kind");
    }
    if (value_len > reader.remaining()) {
      return fail(BodyStatus::Malformed, ErrorType::UnsupportedMetadata,
                  "metadata field value truncated");
    }
    MetadataField field;
    field.id = id;
    field.kind = kind;
    const auto value = reader.bytes(value_len);
    field.value.assign(value.begin(), value.end());
    if (!known_field(id) && kind == FieldKind::Required) {
      return fail(BodyStatus::UnknownRequiredField,
                  ErrorType::UnknownRequiredField,
                  "unknown required metadata field 0x" + std::to_string(id));
    }
    h.metadata.push_back(std::move(field));
  }
  return {};
}

}  // namespace

BodyResult decode_packet_body(std::span<const uint8_t> body,
                              size_t max_frame_bytes, Packet& packet) {
  ByteReader reader(body);
  PacketHeader& h = packet.header;
  try {
    h.meta_version = reader.u16();
    h.meta_flags = reader.u16();
    h.frame_seq = reader.u64();
    h.capture_clock = reader.u32();
    h.capture_hz = reader.u32();
    h.width = reader.u32();
    h.height = reader.u32();
    h.codec = static_cast<CodecId>(reader.u8());
    const uint8_t field_count = reader.u8();
    reader.u16();

    if (h.meta_version != kMetadataVersion) {
      return fail(BodyStatus::Malformed, ErrorType::UnsupportedMetadata,
                  "unsupported metadata version " +
                      std::to_string(h.meta_version));
    }
    if ((h.meta_flags & ~kKnownMetaFlags) != 0) {
      return fail(BodyStatus::Malformed, ErrorType::UnsupportedMetadata,
                  "unsupported metadata flags");
    }
    if (h.codec != CodecId::Png && h.codec != CodecId::Mjpeg) {
      return fail(BodyStatus::Malformed, ErrorType::UnsupportedMetadata,
                  "unsupported codec id " +
                      std::to_string(static_cast<int>(h.codec)));
    }
    if (field_count > kMaxMetadataFields) {
      return fail(BodyStatus::Malformed, ErrorType::UnsupportedMetadata,
                  "metadata field count out of range");
    }
    BodyResult fields = read_fields(reader, field_count, h);
    if (fields.status != BodyStatus::Ok) {
      return fields;
    }

    const uint32_t payload_len = reader.u32();
    if (payload_len > max_frame_bytes) {
      return fail(BodyStatus::LimitExceeded, ErrorType::LimitExceeded,
                  "payload " + std::to_string(payload_len) +
                      " exceeds frame limit " + std::to_string(max_frame_bytes));
    }
    if (!reader.has(payload_len) || !reader.has(kSha256Size)) {
      return fail(BodyStatus::Malformed, ErrorType::UnsupportedMetadata,
                  "payload or checksum truncated");
    }
    const auto payload = reader.bytes(payload_len);
    packet.payload.assign(payload.begin(), payload.end());
    const auto checksum = reader.bytes(kSha256Size);
    std::copy(checksum.begin(), checksum.end(), packet.checksum.begin());
  } catch (const std::out_of_range&) {
    return fail(BodyStatus::Malformed, ErrorType::UnsupportedMetadata,
                "record body truncated");
  }

  const Sha256Digest actual = Sha256::hash(
      std::span<const uint8_t>(packet.payload.data(), packet.payload.size()));
  if (actual != packet.checksum) {
    return fail(BodyStatus::ChecksumMismatch, ErrorType::ChecksumMismatch,
                "payload checksum mismatch");
  }

  return {};
}

}  // namespace mfs
