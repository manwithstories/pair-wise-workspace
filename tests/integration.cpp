#include <catch_amalgamated.hpp>

#include <cmath>
#include <sstream>

#include "color_matrix.hpp"
#include "image_io.hpp"
#include "interpolator.hpp"
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

// ------------------------------------------------------- colour matrices ----
TEST_CASE("colour: same-space matrix is identity") {
  for (const auto space : {lutgrade::ColorSpace::kSrgb, lutgrade::ColorSpace::kRec709,
                           lutgrade::ColorSpace::kRec2020,
                           lutgrade::ColorSpace::kAcescg}) {
    const auto m = lutgrade::conversion_matrix(space, space);
    for (std::size_t i = 0; i < 9; ++i) {
      REQUIRE(m.m[i] == Catch::Approx(i % 4 == 0 ? 1.0 : 0.0).margin(1e-15));
    }
  }
}

TEST_CASE("colour: sRGB and Rec.709 share primaries") {
  const auto m = lutgrade::conversion_matrix(lutgrade::ColorSpace::kSrgb,
                                             lutgrade::ColorSpace::kRec709);
  REQUIRE(m.m[0] == Catch::Approx(1.0).margin(1e-12));
  REQUIRE(m.m[4] == Catch::Approx(1.0).margin(1e-12));
  REQUIRE(m.m[8] == Catch::Approx(1.0).margin(1e-12));
  REQUIRE(std::abs(m.m[1]) < 1e-12);
  REQUIRE(std::abs(m.m[3]) < 1e-12);
}

TEST_CASE("colour: white maps to white and matrices invert") {
  const std::array<double, 3> white{1.0, 1.0, 1.0};
  const auto spaces = {lutgrade::ColorSpace::kSrgb, lutgrade::ColorSpace::kRec709,
                       lutgrade::ColorSpace::kRec2020,
                       lutgrade::ColorSpace::kAcescg};
  for (const auto from : spaces) {
    for (const auto to : spaces) {
      const auto fwd = lutgrade::conversion_matrix(from, to);
      const auto back = lutgrade::conversion_matrix(to, from);
      const auto round = lutgrade::multiply(back, fwd);
      for (std::size_t i = 0; i < 9; ++i) {
        REQUIRE(round.m[i] ==
                Catch::Approx(i % 4 == 0 ? 1.0 : 0.0).margin(1e-9));
      }
      const auto encoded_white = lutgrade::apply(fwd, white);
      for (const double v : encoded_white) {
        REQUIRE(v == Catch::Approx(1.0).margin(1e-9));
      }
    }
  }
}

TEST_CASE("colour: sRGB->Rec.2020 primary shift is directionally correct") {
  // Pure sRGB red must land mostly in Rec.2020 red and below 1.0 in the
  // other channels; sRGB blue converts with a small green component.
  const auto m = lutgrade::conversion_matrix(lutgrade::ColorSpace::kSrgb,
                                             lutgrade::ColorSpace::kRec2020);
  const auto red = lutgrade::apply(m, {1.0, 0.0, 0.0});
  REQUIRE(red[0] == Catch::Approx(0.6274038959).margin(1e-9));
  REQUIRE(red[1] == Catch::Approx(0.0690972894).margin(1e-9));
  REQUIRE(red[2] == Catch::Approx(0.0163914389).margin(1e-9));
  const auto blue = lutgrade::apply(m, {0.0, 0.0, 1.0});
  REQUIRE(blue[2] == Catch::Approx(0.8955952532).margin(1e-9));
  REQUIRE(blue[2] < 1.0);
}

TEST_CASE("colour: transfer round-trips and clamps") {
  for (const auto space : {lutgrade::ColorSpace::kSrgb,
                           lutgrade::ColorSpace::kRec709,
                           lutgrade::ColorSpace::kRec2020}) {
    for (double v = 0.0; v <= 1.0; v += 0.05) {
      CAPTURE(v);
      REQUIRE(lutgrade::encode_transfer(space, lutgrade::decode_transfer(space, v)) ==
              Catch::Approx(v).margin(1e-12));
    }
  }
  REQUIRE(lutgrade::decode_transfer(lutgrade::ColorSpace::kAcescg, 0.25) == 0.25);
  REQUIRE(lutgrade::decode_transfer(lutgrade::ColorSpace::kSrgb, -1.0) == 0.0);
  REQUIRE(lutgrade::encode_transfer(lutgrade::ColorSpace::kSrgb, 2.0) ==
          Catch::Approx(1.0).margin(1e-12));
  // sRGB decode is linear near zero and steepens in the shadows.
  REQUIRE(lutgrade::decode_transfer(lutgrade::ColorSpace::kSrgb, 0.02) ==
          Catch::Approx(0.02 / 12.92).margin(1e-15));
  REQUIRE(lutgrade::decode_transfer(lutgrade::ColorSpace::kSrgb, 0.5) ==
          Catch::Approx(0.21404114048223255).margin(1e-12));
}

TEST_CASE("colour: clamp_linear reports out-of-gamut channels") {
  const auto in_range = lutgrade::clamp_linear({0.2, 0.5, 0.9});
  REQUIRE_FALSE(in_range.clipped_low);
  REQUIRE_FALSE(in_range.clipped_high);
  const auto low = lutgrade::clamp_linear({-0.1, 0.5, 0.9});
  REQUIRE(low.clipped_low);
  REQUIRE(low.rgb[0] == 0.0);
  const auto high = lutgrade::clamp_linear({0.1, 1.4, 0.9});
  REQUIRE(high.clipped_high);
  REQUIRE(high.rgb[1] == 1.0);
  const auto nan = lutgrade::clamp_linear({std::nan(""), 0.5, 0.5});
  REQUIRE(nan.non_finite);
}

TEST_CASE("colour: space names parse case-insensitively") {
  REQUIRE(lutgrade::parse_color_space("sRGB") == lutgrade::ColorSpace::kSrgb);
  REQUIRE(lutgrade::parse_color_space("Rec709") == lutgrade::ColorSpace::kRec709);
  REQUIRE(lutgrade::parse_color_space("REC2020") == lutgrade::ColorSpace::kRec2020);
  REQUIRE(lutgrade::parse_color_space("acescg") == lutgrade::ColorSpace::kAcescg);
  try {
    lutgrade::parse_color_space("adobe");
    FAIL("expected throw");
  } catch (const Error& e) {
    REQUIRE(e.code() == ExitCode::kColor);
    REQUIRE(e.kind() == "space_unknown");
  }
}

// ------------------------------------------------------- interpolation ----
namespace {

// Identity LUT of size n, built directly so tests do not depend on the fixture.
lutgrade::Lut3D identity_lut(int n) {
  lutgrade::Lut3D lut;
  lut.size = n;
  lut.table.reserve(static_cast<std::size_t>(n) * n * n);
  const double d = n - 1.0;
  for (int b = 0; b < n; ++b)
    for (int g = 0; g < n; ++g)
      for (int r = 0; r < n; ++r)
        lut.table.push_back({static_cast<float>(r / d),
                             static_cast<float>(g / d),
                             static_cast<float>(b / d)});
  return lut;
}

}  // namespace

TEST_CASE("interp: identity LUT is transparent at grid points") {
  const auto lut = identity_lut(17);
  for (int i = 0; i < 17; ++i) {
    const double t = i / 16.0;
    const auto out = lutgrade::lut_sample_trilinear(lut, {t, t, t});
    REQUIRE(out[0] == Catch::Approx(t).margin(1e-6));
    REQUIRE(out[2] == Catch::Approx(t).margin(1e-6));
  }
}

TEST_CASE("interp: identity LUT is exact between grid points too") {
  const auto lut = identity_lut(33);
  for (double t = 0.0; t <= 1.0; t += 0.013) {
    CAPTURE(t);
    const auto out = lutgrade::lut_sample_trilinear(lut, {t, 1.0 - t, t * 0.5});
    REQUIRE(out[0] == Catch::Approx(t).margin(1e-6));
    REQUIRE(out[1] == Catch::Approx(1.0 - t).margin(1e-6));
    REQUIRE(out[2] == Catch::Approx(t * 0.5).margin(1e-6));
  }
}

TEST_CASE("interp: trilinear matches a hand-computed midpoint") {
  // 2x2x2 cube with value = r + 2g + 4b at the corners; the centre must be the
  // average of all eight, i.e. 3.5/7 scaled. A 2-grid is built by hand because
  // the parser only accepts 17/33/65.
  lutgrade::Lut3D lut;
  lut.size = 2;
  for (int b = 0; b < 2; ++b)
    for (int g = 0; g < 2; ++g)
      for (int r = 0; r < 2; ++r)
        lut.table.push_back({static_cast<float>(r + 2 * g + 4 * b) / 7.0f,
                             static_cast<float>(r) ,
                             static_cast<float>(b)});
  const auto out = lutgrade::lut_sample_trilinear(lut, {0.5, 0.5, 0.5});
  REQUIRE(out[0] == Catch::Approx(3.5 / 7.0).margin(1e-6));
  REQUIRE(out[1] == Catch::Approx(0.5).margin(1e-6));
  REQUIRE(out[2] == Catch::Approx(0.5).margin(1e-6));
}

TEST_CASE("interp: boundary values clamp to the cube edge") {
  const auto lut = identity_lut(17);
  const auto low = lutgrade::lut_sample_trilinear(lut, {-5.0, -0.2, -1e-9});
  REQUIRE(low[0] == Catch::Approx(0.0).margin(1e-9));
  const auto high = lutgrade::lut_sample_trilinear(lut, {2.0, 1.5, 1.0001});
  REQUIRE(high[0] == Catch::Approx(1.0).margin(1e-6));
}

TEST_CASE("interp: domain mapping is honoured") {
  const auto lut = identity_lut(17);
  lutgrade::Lut3D scoped = lut;
  scoped.domain_min = {0.2, 0.2, 0.2};
  scoped.domain_max = {0.8, 0.8, 0.8};
  // 0.2 is the low edge -> identity grid value 0; 0.8 the high edge -> 1.
  const auto at_min = lutgrade::lut_sample_trilinear(scoped, {0.2, 0.2, 0.2});
  REQUIRE(at_min[0] == Catch::Approx(0.0).margin(1e-6));
  const auto at_max = lutgrade::lut_sample_trilinear(scoped, {0.8, 0.8, 0.8});
  REQUIRE(at_max[0] == Catch::Approx(1.0).margin(1e-6));
  const auto outside = lutgrade::lut_sample_trilinear(scoped, {0.1, 0.1, 0.1});
  REQUIRE(outside[0] == Catch::Approx(0.0).margin(1e-6));
}

TEST_CASE("interp: non-finite input is an error, not a silent NaN") {
  const auto lut = identity_lut(17);
  REQUIRE_THROWS_AS(
      lutgrade::lut_sample_trilinear(lut, {std::nan(""), 0.5, 0.5}),
      Error);
  REQUIRE_THROWS_AS(lutgrade::lut_sample_trilinear(lut, {0.5, INFINITY, 0.5}),
                    Error);
}

TEST_CASE("interp: malformed LUT table is rejected at sample time") {
  lutgrade::Lut3D bad;
  bad.size = 17;  // no entries
  REQUIRE_THROWS_AS(lutgrade::lut_sample_trilinear(bad, {0.5, 0.5, 0.5}), Error);
}
