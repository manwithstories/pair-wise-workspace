#include "media/checksum.hpp"

#include <catch2/catch_test_macros.hpp>

#include <string>
#include <vector>

#include "media/sha256.hpp"

using namespace mediaqc;

namespace {

FrameMetadata frame_with(std::uint64_t seq, const std::vector<std::uint8_t>& data) {
  FrameMetadata metadata;
  metadata.seq = seq;
  metadata.line = seq + 1;
  metadata.offset = seq * 100;
  metadata.sha = sha256(data.data(), data.size());
  return metadata;
}

}  // namespace

TEST_CASE("matching payload passes verification", "[checksum]") {
  std::vector<std::uint8_t> data = {1, 2, 3, 4, 5};
  FrameMetadata metadata = frame_with(0, data);
  FrameVerifier verifier("video_main", 64, 48);
  auto verdict = verifier.verify(metadata, data.data(), data.size());
  CHECK(verdict.check == FrameCheck::kOk);
  CHECK(verifier.verified() == 1);
  CHECK(verifier.damaged() == 0);
}

TEST_CASE("mismatched checksum reports damage", "[checksum]") {
  std::vector<std::uint8_t> data = {1, 2, 3, 4, 5};
  FrameMetadata metadata = frame_with(0, data);
  data[2] ^= 0xFF;  // flip a byte after the checksum was recorded
  FrameVerifier verifier("video_main", 64, 48);
  auto verdict = verifier.verify(metadata, data.data(), data.size());
  CHECK(verdict.check == FrameCheck::kDamaged);
  CHECK(verifier.damaged() == 1);
  CHECK(verifier.verified() == 0);
}

TEST_CASE("duplicate sequence numbers are reported idempotently", "[checksum]") {
  std::vector<std::uint8_t> data = {9, 8, 7};
  FrameMetadata metadata = frame_with(4, data);
  FrameVerifier verifier("video_main", 64, 48);
  auto first = verifier.verify(metadata, data.data(), data.size());
  CHECK(first.check == FrameCheck::kOk);
  auto second = verifier.verify(metadata, data.data(), data.size());
  CHECK(second.check == FrameCheck::kDuplicate);
  CHECK(second.first_index == metadata.line);  // first occurrence recorded
  CHECK(verifier.duplicates() == 1);
  CHECK(verifier.verified() == 1);
}

TEST_CASE("error list serializes with a stable, offset-sorted order",
          "[checksum]") {
  ErrorList list;
  ErrorEntry late;
  late.code = "checksum_mismatch";
  late.track = "video_main";
  late.offset = 2048;
  late.has_offset = true;
  late.message = "payload does not match";
  list.add(late);

  ErrorEntry early;
  early.code = "duplicate_seq";
  early.track = "audio_main";
  early.offset = 512;
  early.has_offset = true;
  early.message = "sequence repeated";
  list.add(early);

  CHECK(list.size() == 2);
  CHECK(list.first_offset() == 512);

  Json array = list.to_json();
  REQUIRE(array.is_array());
  REQUIRE(array.items().size() == 2);
  CHECK(array.items()[0].find("code")->as_string() == "duplicate_seq");
  CHECK(array.items()[1].find("code")->as_string() == "checksum_mismatch");
  CHECK(array.items()[0].find("offset")->as_int() == 512);
}

TEST_CASE("errors from the core model map into entries with severity",
          "[checksum]") {
  ErrorList list;
  Error corruption;
  corruption.category = ErrorCategory::kCorruption;
  corruption.code = "truncated_media";
  corruption.message = "ended early";
  corruption.has_offset = true;
  corruption.offset = 4096;
  list.add(corruption);
  REQUIRE(list.size() == 1);
  CHECK(list.entries()[0].severity == "damage");
  CHECK(list.first_offset() == 4096);
}

TEST_CASE("empty error list reports minus one as first offset", "[checksum]") {
  ErrorList list;
  CHECK(list.first_offset() == -1);
  CHECK(list.empty());
}
