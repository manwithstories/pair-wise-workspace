#include <catch_amalgamated.hpp>

#include <sstream>

#include "image_io.hpp"
#include "lut_parser.hpp"

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

// ---------------------------------------------------------------- .cube ----
namespace {

// Build a syntactically valid identity cube of a given size.
std::string identity_cube(int n, const std::string& extra_header = "") {
  std::ostringstream out;
  out << "TITLE \"identity\"\n" << extra_header;
  out << "LUT_3D_SIZE " << n << "\n";
  for (int b = 0; b < n; ++b) {
    for (int g = 0; g < n; ++g) {
      for (int r = 0; r < n; ++r) {
        out << static_cast<double>(r) / (n - 1) << ' '
            << static_cast<double>(g) / (n - 1) << ' '
            << static_cast<double>(b) / (n - 1) << '\n';
      }
    }
  }
  return out.str();
}

}  // namespace

TEST_CASE("cube: 17/33/65 identity parse with red-fastest ordering") {
  for (const int n : {17, 33, 65}) {
    CAPTURE(n);
    const auto lut = lutgrade::parse_cube(identity_cube(n), "<mem>");
    REQUIRE(lut.size == n);
    REQUIRE(lut.entry_count() == static_cast<std::size_t>(n) * n * n);
    REQUIRE(lut.table.size() == lut.entry_count());
    REQUIRE(lut.title == "identity");
    // First entry is black, last is white, and index follows r + g*N + b*N^2.
    REQUIRE(lut.table.front()[0] == 0.0f);
    REQUIRE(lut.table.back()[0] == 1.0f);
    const std::size_t idx = 1 + 2 * n + 3 * static_cast<std::size_t>(n) * n;
    REQUIRE(lut.table[idx][0] == Catch::Approx(1.0 / (n - 1)));
    REQUIRE(lut.table[idx][1] == Catch::Approx(2.0 / (n - 1)));
    REQUIRE(lut.table[idx][2] == Catch::Approx(3.0 / (n - 1)));
  }
}

TEST_CASE("cube: domain directives and comments") {
  const auto lut = lutgrade::parse_cube(
      "# leading comment\n"
      "LUT_3D_SIZE 17\n"
      "DOMAIN_MIN 0.05 0.06 0.07\n"
      "DOMAIN_MAX 0.95 0.96 0.97  # trailing comment\n" +
          [] {
            std::ostringstream data;
            for (int i = 0; i < 17 * 17 * 17; ++i) data << "0.5 0.5 0.5\n";
            return data.str();
          }(),
      "<mem>");
  REQUIRE(lut.domain_min[0] == Catch::Approx(0.05));
  REQUIRE(lut.domain_min[2] == Catch::Approx(0.07));
  REQUIRE(lut.domain_max[1] == Catch::Approx(0.96));
}

TEST_CASE("cube: missing lines and duplicate declarations are rejected") {
  const std::string data17 = [] {
    std::ostringstream s;
    for (int i = 0; i < 17 * 17 * 17; ++i) s << "0 0 0\n";
    return s.str();
  }();
  REQUIRE_THROWS_WITH(lutgrade::parse_cube(data17, "<mem>"),
                      Catch::Matchers::ContainsSubstring("LUT_3D_SIZE"));
  REQUIRE_THROWS_WITH(
      lutgrade::parse_cube("LUT_3D_SIZE 17\nLUT_3D_SIZE 33\n" + data17, "<mem>"),
      Catch::Matchers::ContainsSubstring("duplicate LUT_3D_SIZE"));
  REQUIRE_THROWS_WITH(
      lutgrade::parse_cube("LUT_3D_SIZE 17\n" + data17 +
                               "DOMAIN_MIN 0 0 0\nDOMAIN_MAX 1 1 1\n",
                           "<mem>"),
      Catch::Matchers::ContainsSubstring("after LUT data"));
}

TEST_CASE("cube: size and value validation") {
  REQUIRE_THROWS_WITH(lutgrade::parse_cube(identity_cube(16), "<mem>"),
                      Catch::Matchers::ContainsSubstring("unsupported grid size"));
  REQUIRE_THROWS_WITH(lutgrade::parse_cube(identity_cube(33) + "0.1 0.2 0.3\n",
                                           "<mem>"),
                      Catch::Matchers::ContainsSubstring("extra LUT data"));
  // One entry short.
  std::string short_cube = identity_cube(17);
  const std::size_t last_nl = short_cube.find_last_of('\n', short_cube.size() - 2);
  short_cube.erase(last_nl + 1);
  REQUIRE_THROWS_WITH(lutgrade::parse_cube(short_cube, "<mem>"),
                      Catch::Matchers::ContainsSubstring("expected 4913"));
}

TEST_CASE("cube: non-finite and out-of-range values are rejected") {
  auto replace_first_data = [](const std::string& value) {
    std::ostringstream out;
    out << "LUT_3D_SIZE 17\n" << value << "\n";
    for (int i = 1; i < 17 * 17 * 17; ++i) out << "0 0 0\n";
    return out.str();
  };
  REQUIRE_THROWS_WITH(lutgrade::parse_cube(replace_first_data("nan 0 0"), "<mem>"),
                      Catch::Matchers::ContainsSubstring("invalid LUT value"));
  REQUIRE_THROWS_WITH(lutgrade::parse_cube(replace_first_data("inf 0 0"), "<mem>"),
                      Catch::Matchers::ContainsSubstring("invalid LUT value"));
  REQUIRE_THROWS_WITH(lutgrade::parse_cube(replace_first_data("1e999 0 0"), "<mem>"),
                      Catch::Matchers::ContainsSubstring("invalid LUT value"));
  REQUIRE_THROWS_WITH(lutgrade::parse_cube(replace_first_data("-0.1 0 0"), "<mem>"),
                      Catch::Matchers::ContainsSubstring("out of [0,1]"));
  REQUIRE_THROWS_WITH(lutgrade::parse_cube(replace_first_data("1.5 0 0"), "<mem>"),
                      Catch::Matchers::ContainsSubstring("out of [0,1]"));
}

TEST_CASE("cube: domain max must exceed min") {
  const std::string data17 = [] {
    std::ostringstream s;
    for (int i = 0; i < 17 * 17 * 17; ++i) s << "0 0 0\n";
    return s.str();
  }();
  REQUIRE_THROWS_WITH(
      lutgrade::parse_cube("LUT_3D_SIZE 17\nDOMAIN_MIN 0.5 0 0\n"
                           "DOMAIN_MAX 0.4 1 1\n" + data17,
                           "<mem>"),
      Catch::Matchers::ContainsSubstring("DOMAIN_MAX must be greater"));
}
