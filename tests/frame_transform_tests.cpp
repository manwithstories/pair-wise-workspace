#include <catch2/catch_test_macros.hpp>

#include <cstdint>
#include <vector>

#include "frame_transform.hpp"
#include "sha256.hpp"

using namespace mfs;

namespace {

constexpr uint32_t kWidth = 96;
constexpr uint32_t kHeight = 54;

std::vector<uint8_t> make_image(uint32_t seed) {
  std::vector<uint8_t> rgb(static_cast<size_t>(kWidth) * kHeight * 3);
  for (uint32_t y = 0; y < kHeight; ++y) {
    for (uint32_t x = 0; x < kWidth; ++x) {
      const size_t at = (static_cast<size_t>(y) * kWidth + x) * 3;
      rgb[at + 0] = static_cast<uint8_t>((x + seed) & 0xFF);
      rgb[at + 1] = static_cast<uint8_t>((y * 2 + seed) & 0xFF);
      rgb[at + 2] = static_cast<uint8_t>((x ^ y) & 0xFF);
    }
  }
  return rgb;
}

}  // namespace

TEST_CASE("PNG payloads encode and decode with a stable digest",
          "[frame_transform]") {
  FrameTransform transform;
  const std::vector<uint8_t> image = make_image(17);
  std::vector<uint8_t> png;
  REQUIRE(transform.encode_png(image, kWidth, kHeight, png));
  REQUIRE(png.size() > 8);

  const DecodeOutcome outcome = transform.decode(CodecId::Png, png, kWidth, kHeight);
  CHECK(outcome.ok);
  CHECK(outcome.width == kWidth);
  CHECK(outcome.height == kHeight);
  CHECK(outcome.content_sha256 == Sha256::hash(image));

  // Decoding is deterministic: the same payload yields the same digest.
  const DecodeOutcome again = transform.decode(CodecId::Png, png, kWidth, kHeight);
  CHECK(again.ok);
  CHECK(again.content_sha256 == outcome.content_sha256);
}

TEST_CASE("different images hash differently", "[frame_transform]") {
  FrameTransform transform;
  std::vector<uint8_t> first;
  std::vector<uint8_t> second;
  REQUIRE(transform.encode_png(make_image(1), kWidth, kHeight, first));
  REQUIRE(transform.encode_png(make_image(2), kWidth, kHeight, second));
  const auto a = transform.decode(CodecId::Png, first, kWidth, kHeight);
  const auto b = transform.decode(CodecId::Png, second, kWidth, kHeight);
  CHECK(a.ok);
  CHECK(b.ok);
  CHECK(a.content_sha256 != b.content_sha256);
}

TEST_CASE("truncated payloads fail without throwing", "[frame_transform]") {
  FrameTransform transform;
  std::vector<uint8_t> png;
  REQUIRE(transform.encode_png(make_image(3), kWidth, kHeight, png));
  const std::vector<uint8_t> half(png.begin(), png.begin() + png.size() / 2);
  const DecodeOutcome outcome =
      transform.decode(CodecId::Png, half, kWidth, kHeight);
  CHECK_FALSE(outcome.ok);
  CHECK_FALSE(outcome.detail.empty());
}

TEST_CASE("garbage payloads fail without throwing", "[frame_transform]") {
  FrameTransform transform;
  const std::vector<uint8_t> garbage(512, 0xAB);
  const DecodeOutcome outcome =
      transform.decode(CodecId::Png, garbage, kWidth, kHeight);
  CHECK_FALSE(outcome.ok);

  const DecodeOutcome empty = transform.decode(CodecId::Png, {}, kWidth, kHeight);
  CHECK_FALSE(empty.ok);
}

TEST_CASE("a geometry mismatch is reported", "[frame_transform]") {
  FrameTransform transform;
  std::vector<uint8_t> png;
  REQUIRE(transform.encode_png(make_image(4), kWidth, kHeight, png));
  const DecodeOutcome outcome =
      transform.decode(CodecId::Png, png, kWidth + 2, kHeight);
  CHECK_FALSE(outcome.ok);
  CHECK(outcome.detail.find("differs from declared") != std::string::npos);
}

TEST_CASE("encoder rejects inconsistent buffers", "[frame_transform]") {
  FrameTransform transform;
  std::vector<uint8_t> png;
  std::vector<uint8_t> wrong_size(10, 0);
  CHECK_FALSE(transform.encode_png(wrong_size, kWidth, kHeight, png));
  CHECK(png.empty());
  CHECK_FALSE(transform.encode_png(make_image(0), 0, kHeight, png));
  CHECK(png.empty());
}
