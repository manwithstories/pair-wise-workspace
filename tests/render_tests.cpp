#include <catch_amalgamated.hpp>

#include <cmath>
#include <string>

#include "compare.hpp"
#include "fixture.hpp"
#include "image_io.hpp"
#include "interpolator.hpp"
#include "lut_parser.hpp"
#include "renderer_scalar.hpp"
#include "renderer_tiled.hpp"
#include "statistics.hpp"

using namespace lutgrade;

namespace {

Lut3D identity_lut(int n) {
  Lut3D lut;
  lut.size = n;
  const double d = n - 1.0;
  for (int b = 0; b < n; ++b)
    for (int g = 0; g < n; ++g)
      for (int r = 0; r < n; ++r)
        lut.table.push_back({static_cast<float>(r / d),
                             static_cast<float>(g / d),
                             static_cast<float>(b / d)});
  return lut;
}

GradeSettings settings_for(ColorSpace in, ColorSpace out) {
  GradeSettings s;
  s.in_space = in;
  s.out_space = out;
  s.matrix = conversion_matrix(in, out);
  return s;
}

// A LUT whose table size disagrees with `size` makes the kernel throw, which is
// how the abort path is exercised without depending on I/O failures.
Lut3D broken_lut() {
  Lut3D lut = identity_lut(17);
  lut.table.resize(100);  // far fewer than 17^3
  return lut;
}

}  // namespace

TEST_CASE("render: scalar and tiled agree bit-for-bit across thread counts") {
  const Image input = make_fixture_image();
  const Lut3D lut = identity_lut(33);
  const GradingKernel kernel(&lut, settings_for(ColorSpace::kSrgb,
                                                ColorSpace::kRec709));
  const RenderOutput scalar = render_scalar(input, kernel);
  for (const int threads : {1, 2, 3, 4, 8}) {
    CAPTURE(threads);
    TiledOptions options;
    options.threads = threads;
    const RenderOutput tiled = render_tiled(input, kernel, options);
    REQUIRE(tiled.after == scalar.after);
    REQUIRE(tiled.before == scalar.before);
    REQUIRE(tiled.clipped_pixels == scalar.clipped_pixels);
  }
}

TEST_CASE("render: output ordering is row-major regardless of tiling") {
  // 70x70 crosses six tiles per axis with 32px tiles; pixel (69,69) must still
  // land at the last slot on both paths.
  Image input;
  input.width = 70;
  input.height = 70;
  input.rgb.resize(input.pixel_count() * 3, 0);
  const std::size_t last = (input.pixel_count() - 1) * 3;
  input.rgb[last + 0] = 255;
  input.rgb[last + 1] = 128;
  input.rgb[last + 2] = 0;

  const Lut3D lut = identity_lut(17);
  const GradingKernel kernel(&lut, settings_for(ColorSpace::kSrgb,
                                                ColorSpace::kSrgb));
  const RenderOutput scalar = render_scalar(input, kernel);
  TiledOptions options;
  options.threads = 6;
  const RenderOutput tiled = render_tiled(input, kernel, options);

  REQUIRE(tiled.after == scalar.after);
  REQUIRE(scalar.after[last + 0] == Catch::Approx(1.0f).margin(1e-6));
  REQUIRE(scalar.after[last + 1] == Catch::Approx(128 / 255.0f).margin(1e-6));
}

TEST_CASE("render: a failing tile aborts the whole pass") {
  const Image input = make_fixture_image();
  const Lut3D lut = broken_lut();
  const GradingKernel kernel(&lut, settings_for(ColorSpace::kSrgb,
                                                ColorSpace::kSrgb));
  REQUIRE_THROWS_AS(render_tiled(input, kernel, TiledOptions{4}), Error);
  REQUIRE_THROWS_AS(render_scalar(input, kernel), Error);
  try {
    render_tiled(input, kernel, TiledOptions{4});
    FAIL("expected throw");
  } catch (const Error& e) {
    REQUIRE(e.code() == ExitCode::kRender);
    REQUIRE(e.message().find("tile") != std::string::npos);
  }
}

TEST_CASE("render: zero threads is a usage error") {
  const Image input = make_fixture_image();
  const Lut3D lut = identity_lut(17);
  const GradingKernel kernel(&lut, settings_for(ColorSpace::kSrgb,
                                                ColorSpace::kSrgb));
  REQUIRE_THROWS_AS(render_tiled(input, kernel, TiledOptions{0}), Error);
}

TEST_CASE("stats: identity grade has zero deviation and matching hashes") {
  const Image input = make_fixture_image();
  const Lut3D lut = identity_lut(33);
  const GradingKernel kernel(&lut, settings_for(ColorSpace::kSrgb,
                                                ColorSpace::kSrgb));
  const RenderOutput rendered = render_scalar(input, kernel);
  const Statistics stats = compute_statistics(rendered);

  REQUIRE(stats.pixel_count == input.pixel_count());
  // Identity LUT + identity matrix: only float round-trip error remains.
  REQUIRE(stats.deviation[0].max_abs < 1e-6);
  REQUIRE(stats.mean_deviation < 1e-6);

  std::uint64_t before_total = 0;
  std::uint64_t after_total = 0;
  for (std::size_t c = 0; c < 3; ++c) {
    for (std::size_t i = 0; i < kHistogramBins; ++i) {
      before_total += stats.hist_before[c][i];
      after_total += stats.hist_after[c][i];
    }
  }
  REQUIRE(before_total == input.pixel_count() * 3);
  REQUIRE(after_total == input.pixel_count() * 3);
  REQUIRE(format_hash(stats.hash).size() == 16);
}

TEST_CASE("stats: hash is stable and sensitive to pixel changes") {
  const Image input = make_fixture_image();
  const Lut3D lut = identity_lut(17);
  const GradingKernel kernel(&lut, settings_for(ColorSpace::kSrgb,
                                                ColorSpace::kSrgb));
  const auto a = compute_statistics(render_scalar(input, kernel));
  const auto b = compute_statistics(render_scalar(input, kernel));
  REQUIRE(a.hash == b.hash);

  Image tweaked = input;
  tweaked.rgb[0] ^= 0x01;
  const auto c = compute_statistics(render_scalar(tweaked, kernel));
  REQUIRE(c.hash != a.hash);
}

TEST_CASE("stats: an out-of-gamut conversion counts clipped pixels") {
  // sRGB white -> sRGB->ACEScg round trip stays in range, but a pure Rec.2020
  // green mapped straight into sRGB is well outside sRGB's gamut and must be
  // flagged, not silently dropped.
  Image input;
  input.width = 1;
  input.height = 1;
  input.rgb = {0, 255, 0};
  const Lut3D lut = identity_lut(17);
  const GradingKernel kernel(&lut, settings_for(ColorSpace::kRec2020,
                                                ColorSpace::kSrgb));
  const RenderOutput rendered = render_scalar(input, kernel);
  REQUIRE(rendered.clipped_pixels == 1);
}

TEST_CASE("fixture: generation is deterministic") {
  const Image a = make_fixture_image();
  const Image b = make_fixture_image();
  REQUIRE(a.rgb == b.rgb);
  const std::string cube1 = make_demo_cube(33, "demo");
  const std::string cube2 = make_demo_cube(33, "demo");
  REQUIRE(cube1 == cube2);
  // Re-parsing the generated cube must succeed and match the grid size.
  const Lut3D parsed = parse_cube(cube1, "<mem>");
  REQUIRE(parsed.size == 33);
}

TEST_CASE("fixture: demo cube parses at every supported size") {
  for (const int n : {17, 33, 65}) {
    CAPTURE(n);
    const Lut3D parsed = parse_cube(make_demo_cube(n, "t"), "<mem>");
    REQUIRE(parsed.size == n);
    REQUIRE(parsed.table.size() == parsed.entry_count());
  }
}

TEST_CASE("fixture: quantize rounds and clamps") {
  RenderOutput rendered;
  rendered.width = 1;
  rendered.height = 1;
  rendered.after = {-0.4f, 0.5f, 1.7f};
  const Image q = quantize(rendered);
  REQUIRE(q.rgb[0] == 0);
  REQUIRE(q.rgb[1] == 128);
  REQUIRE(q.rgb[2] == 255);
}

// ------------------------------------------------------------- compare ----
TEST_CASE("compare: identical renders match with zero diff") {
  const Image input = make_fixture_image();
  const Lut3D lut = identity_lut(17);
  const GradingKernel kernel(&lut, settings_for(ColorSpace::kSrgb,
                                                ColorSpace::kSrgb));
  const RenderOutput a = render_scalar(input, kernel);
  const RenderOutput b = render_scalar(input, kernel);
  const CompareReport report =
      compare_rendered(a, b, input.width, 1e-9);
  REQUIRE(report.match);
  REQUIRE(report.max_diff == 0.0);
  REQUIRE_FALSE(report.has_first_diff);
  REQUIRE(report.hash_scalar == report.hash_tiled);
}

TEST_CASE("compare: a single perturbed channel reports its coordinate") {
  const Image input = make_fixture_image();
  const Lut3D lut = identity_lut(17);
  const GradingKernel kernel(&lut, settings_for(ColorSpace::kSrgb,
                                                ColorSpace::kSrgb));
  const RenderOutput base = render_scalar(input, kernel);
  RenderOutput tweaked = base;

  // Perturb pixel (5, 7), green channel only.
  const std::size_t pixel = 7 * static_cast<std::size_t>(input.width) + 5;
  tweaked.after[pixel * 3 + 1] += 0.25f;

  const CompareReport report =
      compare_rendered(base, tweaked, input.width, 1e-6);
  REQUIRE_FALSE(report.match);
  REQUIRE(report.has_first_diff);
  REQUIRE(report.first_x == 5);
  REQUIRE(report.first_y == 7);
  REQUIRE(report.first_channel == 1);
  REQUIRE(report.max_diff == Catch::Approx(0.25).margin(1e-6));
  REQUIRE(report.left != report.right);
}

TEST_CASE("compare: differences below threshold still match") {
  const Image input = make_fixture_image();
  const Lut3D lut = identity_lut(17);
  const GradingKernel kernel(&lut, settings_for(ColorSpace::kSrgb,
                                                ColorSpace::kSrgb));
  const RenderOutput base = render_scalar(input, kernel);
  RenderOutput tweaked = base;
  tweaked.after[0] += 1e-8f;
  const CompareReport report = compare_rendered(base, tweaked, input.width, 1e-6);
  REQUIRE(report.match);
  REQUIRE_FALSE(report.has_first_diff);
}
