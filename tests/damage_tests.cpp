#include <catch2/catch_test_macros.hpp>

#include <vector>

#include "damage_builder.hpp"
#include "packet_reader.hpp"
#include "sample_generator.hpp"

using namespace mfs;

namespace {

std::vector<uint8_t> sample_container() {
  GeneratorOptions options;
  options.seed = 3;
  options.frames = 60;
  options.width = 48;
  options.height = 27;
  options.keyframe_interval = 10;
  options.loss_events = 1;
  options.loss_burst = 2;
  options.duplicate_rate = 0.05;
  options.reorder_rate = 0.1;
  options.reorder_window = 6;
  options.timestamp_defects = 2;
  std::vector<Packet> packets;
  generate_sample(options, packets);
  return encode_container(packets, 3);
}

// Parses the damaged container and returns the typed parser errors.
std::vector<RepairError> parse_errors(const std::vector<uint8_t>& bytes) {
  PacketReader reader(bytes);
  Packet packet;
  while (reader.next(packet)) {
  }
  return reader.errors();
}

}  // namespace

TEST_CASE("every damage kind produces its intended defect", "[damage]") {
  const auto source = sample_container();

  SECTION("checksum") {
    const DamageResult damaged = apply_damage(source, DamageKind::ChecksumMismatch);
    const auto errors = parse_errors(damaged.bytes);
    REQUIRE(errors.size() == 1);
    CHECK(errors[0].type == ErrorType::ChecksumMismatch);
    CHECK(errors[0].offset == damaged.offset);
  }
  SECTION("length overflow") {
    const DamageResult damaged = apply_damage(source, DamageKind::LengthOverflow);
    const auto errors = parse_errors(damaged.bytes);
    REQUIRE(!errors.empty());
    CHECK(errors[0].type == ErrorType::LengthOverflow);
    CHECK(errors[0].offset == damaged.offset);
  }
  SECTION("truncated tail") {
    const DamageResult damaged = apply_damage(source, DamageKind::TruncatedTail);
    const auto errors = parse_errors(damaged.bytes);
    REQUIRE(!errors.empty());
    CHECK(errors.back().type == ErrorType::RecordTruncated);
    CHECK(errors.back().offset == damaged.offset);
  }
  SECTION("bad magic") {
    const DamageResult damaged = apply_damage(source, DamageKind::BadMagic);
    const auto errors = parse_errors(damaged.bytes);
    REQUIRE(!errors.empty());
    CHECK(errors[0].type == ErrorType::RecordMagicMismatch);
    CHECK(errors[0].offset == damaged.offset);
  }
  SECTION("unknown required field") {
    const DamageResult damaged = apply_damage(source, DamageKind::UnknownRequired);
    const auto errors = parse_errors(damaged.bytes);
    REQUIRE(errors.size() == 1);
    CHECK(errors[0].type == ErrorType::UnknownRequiredField);
    CHECK(errors[0].offset == damaged.offset);
  }
  SECTION("truncated header") {
    const DamageResult damaged = apply_damage(source, DamageKind::TruncatedHeader);
    CHECK(damaged.bytes.size() < kFileHeaderSize);
    const auto errors = parse_errors(damaged.bytes);
    REQUIRE(errors.size() == 1);
    CHECK(errors[0].type == ErrorType::FileHeaderTruncated);
  }
  SECTION("empty sequence") {
    const DamageResult damaged = apply_damage(source, DamageKind::EmptySequence);
    CHECK(damaged.bytes.size() == kFileHeaderSize);
    CHECK(parse_errors(damaged.bytes).empty());
  }
}

TEST_CASE("damage is applied to a copy of the source", "[damage]") {
  const auto source = sample_container();
  const auto original = source;
  const DamageResult damaged = apply_damage(source, DamageKind::ChecksumMismatch);
  CHECK(source == original);
  CHECK(damaged.bytes != source);
}

TEST_CASE("a source without enough records is rejected", "[damage]") {
  std::vector<uint8_t> tiny(kFileHeaderSize, 0);
  tiny[0] = 0x4D;
  tiny[1] = 0x46;
  tiny[2] = 0x53;
  tiny[3] = 0x31;
  CHECK_THROWS_AS(apply_damage(tiny, DamageKind::BadMagic), MediaError);
}
