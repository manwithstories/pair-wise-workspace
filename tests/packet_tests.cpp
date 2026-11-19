#include <catch2/catch_test_macros.hpp>

#include <string>
#include <vector>

#include "byte_io.hpp"
#include "media_types.hpp"
#include "packet_reader.hpp"
#include "packet_writer.hpp"
#include "sha256.hpp"

using namespace mfs;

namespace {

Packet make_packet(uint64_t seq, bool keyframe = false) {
  Packet packet;
  packet.header.frame_seq = seq;
  packet.header.capture_clock = static_cast<uint32_t>(seq * 33333 + 1000);
  packet.header.capture_hz = 1000000;
  packet.header.width = 320;
  packet.header.height = 180;
  packet.header.codec = CodecId::Png;
  packet.header.meta_flags = keyframe ? kFlagKeyframe : 0;
  packet.payload.assign(64, static_cast<uint8_t>(seq & 0xFF));
  packet.header.metadata.push_back(
      MetadataField{kFieldCameraId, FieldKind::Optional, {'c', 'a', 'm', '1'}});
  return packet;
}

std::vector<uint8_t> make_file(const std::vector<Packet>& packets) {
  FileHeader header;
  header.stream_id = 7;
  header.nominal_hz = 1000000;
  std::vector<uint8_t> bytes = encode_file_header(header);
  for (const Packet& packet : packets) {
    const auto record = encode_record(packet);
    bytes.insert(bytes.end(), record.begin(), record.end());
  }
  return bytes;
}

std::vector<Packet> parse_all(std::span<const uint8_t> bytes) {
  PacketReader reader(bytes);
  std::vector<Packet> packets;
  Packet packet;
  while (reader.next(packet)) {
    packets.push_back(packet);
  }
  return packets;
}

}  // namespace

TEST_CASE("packets round-trip through the container", "[packet_reader]") {
  const std::vector<Packet> original{make_packet(1), make_packet(2, true),
                                     make_packet(3)};
  const auto bytes = make_file(original);

  PacketReader reader(bytes);
  std::vector<Packet> parsed;
  Packet packet;
  while (reader.next(packet)) {
    parsed.push_back(packet);
  }

  REQUIRE(parsed.size() == original.size());
  CHECK(reader.errors().empty());
  CHECK_FALSE(reader.truncated());
  for (size_t i = 0; i < original.size(); ++i) {
    CHECK(parsed[i].header.frame_seq == original[i].header.frame_seq);
    CHECK(parsed[i].payload == original[i].payload);
    CHECK(parsed[i].header.metadata == original[i].header.metadata);
    CHECK(encode_packet_body(parsed[i]) == encode_packet_body(original[i]));
  }
  CHECK(parsed[0].raw_offset == kFileHeaderSize);
  CHECK(parsed[1].raw_offset ==
        kFileHeaderSize + 8 + encode_packet_body(original[0]).size());
}

TEST_CASE("unknown optional fields are preserved", "[packet_reader]") {
  Packet packet = make_packet(5);
  packet.header.metadata.push_back(
      MetadataField{0x7001, FieldKind::Optional, {0xDE, 0xAD, 0xBE, 0xEF}});
  const auto bytes = make_file({packet});

  PacketReader reader(bytes);
  Packet parsed;
  REQUIRE(reader.next(parsed));
  REQUIRE(parsed.header.metadata.size() == 2);
  CHECK(parsed.header.metadata[1].id == 0x7001);
  CHECK(parsed.header.metadata[1].kind == FieldKind::Optional);
  CHECK(parsed.header.metadata[1].value ==
        std::vector<uint8_t>{0xDE, 0xAD, 0xBE, 0xEF});
}

TEST_CASE("unknown required fields reject only that packet", "[packet_reader]") {
  Packet good = make_packet(1);
  Packet bad = make_packet(2);
  bad.header.metadata.push_back(MetadataField{0x7002, FieldKind::Required, {1}});
  Packet tail = make_packet(3);
  const auto bytes = make_file({good, bad, tail});

  PacketReader reader(bytes);
  std::vector<uint64_t> seqs;
  Packet packet;
  while (reader.next(packet)) {
    seqs.push_back(packet.header.frame_seq);
  }
  CHECK(seqs == std::vector<uint64_t>{1, 3});
  REQUIRE(reader.errors().size() == 1);
  CHECK(reader.errors()[0].type == ErrorType::UnknownRequiredField);
  CHECK(reader.errors()[0].offset > 0);
  CHECK(reader.errors()[0].record_index == 1);
}

TEST_CASE("checksum mismatch is reported with its offset and skipped",
          "[packet_reader]") {
  auto bytes = make_file({make_packet(1), make_packet(2), make_packet(3)});
  // Flip one payload byte inside the second record.
  const size_t second_offset =
      kFileHeaderSize + encode_record(make_packet(1)).size();
  // Fixed header (32) + one metadata field (8-byte header + 4-byte value) +
  // payload length field (4) lands exactly on the first payload byte.
  bytes[second_offset + 8 + 32 + 12 + 4] ^= 0x5A;

  PacketReader reader(bytes);
  std::vector<uint64_t> seqs;
  Packet packet;
  while (reader.next(packet)) {
    seqs.push_back(packet.header.frame_seq);
  }
  CHECK(seqs == std::vector<uint64_t>{1, 3});
  REQUIRE(reader.errors().size() == 1);
  CHECK(reader.errors()[0].type == ErrorType::ChecksumMismatch);
  CHECK(reader.errors()[0].offset == second_offset);
}

TEST_CASE("truncated header is fatal and reported at offset zero",
          "[packet_reader]") {
  const auto full = make_file({make_packet(1)});
  const std::vector<uint8_t> truncated(full.begin(), full.begin() + 20);
  PacketReader reader(truncated);
  Packet packet;
  CHECK_FALSE(reader.next(packet));
  REQUIRE(reader.errors().size() == 1);
  CHECK(reader.errors()[0].type == ErrorType::FileHeaderTruncated);
  CHECK(reader.errors()[0].offset == 0);
  CHECK(reader.truncated());
}

TEST_CASE("truncated record reports offset and keeps earlier packets",
          "[packet_reader]") {
  auto full = make_file({make_packet(1), make_packet(2)});
  const size_t second_offset =
      kFileHeaderSize + encode_record(make_packet(1)).size();
  full.resize(second_offset + 16);  // cut inside the second record

  PacketReader reader(full);
  std::vector<uint64_t> seqs;
  Packet packet;
  while (reader.next(packet)) {
    seqs.push_back(packet.header.frame_seq);
  }
  CHECK(seqs == std::vector<uint64_t>{1});
  REQUIRE(!reader.errors().empty());
  CHECK(reader.errors().back().type == ErrorType::RecordTruncated);
  CHECK(reader.errors().back().offset == second_offset);
  CHECK(reader.truncated());
}

TEST_CASE("length overflow is rejected without consuming later records",
          "[packet_reader]") {
  auto bytes = make_file({make_packet(1), make_packet(2), make_packet(3)});
  const size_t second_offset =
      kFileHeaderSize + encode_record(make_packet(1)).size();
  // Overwrite the length field with an absurd value.
  bytes[second_offset + 4] = 0xFF;
  bytes[second_offset + 5] = 0xFF;
  bytes[second_offset + 6] = 0xFF;
  bytes[second_offset + 7] = 0xFF;

  PacketReader reader(bytes);
  std::vector<uint64_t> seqs;
  Packet packet;
  while (reader.next(packet)) {
    seqs.push_back(packet.header.frame_seq);
  }
  CHECK(seqs == std::vector<uint64_t>{1, 3});
  REQUIRE(reader.errors().size() == 1);
  CHECK(reader.errors()[0].type == ErrorType::LengthOverflow);
  CHECK(reader.errors()[0].offset == second_offset);
}

TEST_CASE("bad record magic resynchronises", "[packet_reader]") {
  auto bytes = make_file({make_packet(1), make_packet(2), make_packet(3)});
  const size_t second_offset =
      kFileHeaderSize + encode_record(make_packet(1)).size();
  bytes[second_offset] = 0x00;

  PacketReader reader(bytes);
  std::vector<uint64_t> seqs;
  Packet packet;
  while (reader.next(packet)) {
    seqs.push_back(packet.header.frame_seq);
  }
  CHECK(seqs == std::vector<uint64_t>{1, 3});
  CHECK_FALSE(reader.errors().empty());
  CHECK(reader.errors()[0].type == ErrorType::RecordMagicMismatch);
}

TEST_CASE("empty sequence parses to zero packets", "[packet_reader]") {
  const auto bytes = make_file({});
  PacketReader reader(bytes);
  Packet packet;
  CHECK_FALSE(reader.next(packet));
  CHECK(reader.errors().empty());
  CHECK_FALSE(reader.truncated());
}

TEST_CASE("byte helpers round-trip", "[byte_io]") {
  std::vector<uint8_t> out;
  put_u8(out, 0x12);
  put_u16(out, 0x3456);
  put_u32(out, 0x789ABCDE);
  put_u64(out, 0x0123456789ABCDEFull);
  ByteReader reader(out);
  CHECK(reader.u8() == 0x12);
  CHECK(reader.u16() == 0x3456);
  CHECK(reader.u32() == 0x789ABCDE);
  CHECK(reader.u64() == 0x0123456789ABCDEFull);
  CHECK(reader.remaining() == 0);
}
