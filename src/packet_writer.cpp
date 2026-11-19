#include "packet_writer.hpp"

#include "byte_io.hpp"
#include "sha256.hpp"

namespace mfs {
namespace {

// Body layout (after the 8-byte record prefix magic+length):
//   u16 meta_version, u16 meta_flags, u64 frame_seq,
//   u32 capture_clock, u32 capture_hz, u32 width, u32 height,
//   u8 codec, u8 field_count, u16 reserved, fields..., payload..., sha256[32]
void append_field(std::vector<uint8_t>& out, const MetadataField& field) {
  put_u16(out, field.id);
  put_u8(out, static_cast<uint8_t>(field.kind));
  put_u8(out, 0);
  put_u32(out, static_cast<uint32_t>(field.value.size()));
  put_bytes(out, field.value);
}

}  // namespace

std::vector<uint8_t> encode_file_header(const FileHeader& header) {
  std::vector<uint8_t> out;
  out.reserve(kFileHeaderSize);
  put_u32(out, kFileMagic);
  put_u16(out, header.version);
  put_u16(out, 0);
  put_u32(out, header.flags);
  put_u64(out, header.stream_id);
  put_u32(out, header.nominal_hz);
  put_u32(out, 0);
  put_u32(out, 0);
  return out;
}

std::vector<uint8_t> encode_packet_body(const Packet& packet) {
  const PacketHeader& h = packet.header;
  std::vector<uint8_t> out;
  out.reserve(32 + packet.payload.size() + kSha256Size);
  put_u16(out, h.meta_version);
  put_u16(out, h.meta_flags);
  put_u64(out, h.frame_seq);
  put_u32(out, h.capture_clock);
  put_u32(out, h.capture_hz);
  put_u32(out, h.width);
  put_u32(out, h.height);
  put_u8(out, static_cast<uint8_t>(h.codec));
  put_u8(out, static_cast<uint8_t>(h.metadata.size()));
  put_u16(out, 0);
  for (const MetadataField& field : h.metadata) {
    append_field(out, field);
  }
  put_u32(out, static_cast<uint32_t>(packet.payload.size()));
  put_bytes(out, packet.payload);
  const Sha256Digest checksum = Sha256::hash(packet.payload);
  put_bytes(out, std::span<const uint8_t>(checksum.data(), checksum.size()));
  return out;
}

std::vector<uint8_t> encode_record(const Packet& packet) {
  std::vector<uint8_t> body = encode_packet_body(packet);
  std::vector<uint8_t> out;
  out.reserve(8 + body.size());
  put_u32(out, kPacketMagic);
  put_u32(out, static_cast<uint32_t>(body.size()));
  put_bytes(out, body);
  return out;
}

}  // namespace mfs
