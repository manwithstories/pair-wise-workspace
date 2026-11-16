#include <catch_amalgamated.hpp>

#include "image_io.hpp"

using lutgrade::decode_ppm;
using lutgrade::encode_ppm;
using lutgrade::Error;
using lutgrade::ExitCode;

TEST_CASE("PPM round-trip preserves pixels") {
  lutgrade::Image image;
  image.width = 2;
  image.height = 2;
  image.rgb = {0, 1, 2, 253, 254, 255, 10, 20, 30, 40, 50, 60};
  const std::string bytes = encode_ppm(image);
  const auto back = decode_ppm(bytes, "<mem>");
  REQUIRE(back.width == 2);
  REQUIRE(back.height == 2);
  REQUIRE(back.rgb == image.rgb);
}

TEST_CASE("PPM header comments and CRLF are tolerated") {
  std::string bytes = "P6\r\n# vendor comment\r\n1 1\r\n# another\n255\r\n";
  bytes.push_back(static_cast<char>(7));
  bytes.push_back(static_cast<char>(8));
  bytes.push_back(static_cast<char>(9));
  const auto image = decode_ppm(bytes, "<mem>");
  REQUIRE(image.rgb == std::vector<std::uint8_t>{7, 8, 9});
}

TEST_CASE("PPM rejects short/overlong payloads") {
  // Built explicitly: a C-string literal would truncate at the NUL byte.
  const std::string header = "P6 1 1 255 ";
  std::string short_payload = header;
  short_payload.push_back(char(0xFF));
  short_payload.push_back(char(0xFE));
  std::string overlong = header;
  overlong.append(4, char(0xFF));
  REQUIRE_THROWS_AS(decode_ppm(short_payload, "<mem>"), Error);
  REQUIRE_THROWS_AS(decode_ppm(overlong, "<mem>"), Error);
}

TEST_CASE("PPM rejects bad magic, maxval, and zero size") {
  REQUIRE_THROWS_AS(decode_ppm("P3 1 1 255 \0\0\0", "<mem>"), Error);
  REQUIRE_THROWS_AS(decode_ppm("P6 1 1 65535 \0\0\0", "<mem>"), Error);
  REQUIRE_THROWS_AS(decode_ppm("P6 0 1 255 ", "<mem>"), Error);
}

TEST_CASE("PPM errors carry usage exit code") {
  try {
    decode_ppm("not a ppm", "<mem>");
    FAIL("expected throw");
  } catch (const Error& e) {
    REQUIRE(e.code() == ExitCode::kUsage);
    REQUIRE(e.kind() == "ppm_invalid");
  }
}
