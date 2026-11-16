#include "commands.hpp"

#include <algorithm>
#include <cmath>
#include <cstdio>

#include "fixture.hpp"
#include "image_io.hpp"
#include "interpolator.hpp"
#include "lut_parser.hpp"
#include "renderer_scalar.hpp"
#include "renderer_tiled.hpp"
#include "report.hpp"
#include "statistics.hpp"

namespace lutgrade {
namespace {

GradeSettings make_settings(const ColorOptions& color) {
  GradeSettings settings;
  settings.in_space = color.in_space;
  settings.out_space = color.out_space;
  settings.matrix = conversion_matrix(color.in_space, color.out_space);
  return settings;
}

}  // namespace

int execute_fixture(const FixtureOptions& options) {
  generate_fixtures(options.dir, options.size);
  std::printf("FIXTURE_DIR=%s\n", options.dir.c_str());
  std::printf("GRID_SIZE=%d\n", options.size);
  std::printf("INPUT=%s/input.ppm\n", options.dir.c_str());
  std::printf("LUT=%s/demo.cube\n", options.dir.c_str());
  std::printf("WIDTH=%d\n", kFixtureWidth);
  std::printf("HEIGHT=%d\n", kFixtureHeight);
  std::printf("PIXELS=%d\n", kFixtureWidth * kFixtureHeight);
  return static_cast<int>(ExitCode::kOk);
}

int execute_render(const RenderOptions& options) {
  const Image input = read_ppm(options.color.input);
  const Lut3D lut = load_cube(options.color.lut);
  const GradeSettings settings = make_settings(options.color);
  const GradingKernel kernel(&lut, settings);

  RenderOutput rendered;
  if (options.tiled) {
    TiledOptions tiled;
    tiled.threads = options.color.threads;
    rendered = render_tiled(input, kernel, tiled);
  } else {
    rendered = render_scalar(input, kernel);
  }

  // Only write once rendering succeeded; a failed tile walk throws above and
  // leaves no partial file behind.
  write_ppm(options.output, quantize(rendered));

  RunReport report;
  report.grid_size = lut.size;
  report.pixel_count = rendered.pixel_count();
  report.path = options.tiled ? "tiled" : "scalar";
  report.threads = options.tiled ? options.color.threads : 1;
  report.in_space = color_space_name(options.color.in_space);
  report.out_space = color_space_name(options.color.out_space);
  report.stats = compute_statistics(rendered);

  print_stage_log(kernel.settings());
  print_run_report(report);
  std::printf("OUTPUT=%s\n", options.output.c_str());
  return static_cast<int>(ExitCode::kOk);
}

int execute_compare(const CompareOptions& options) {
  const Image input = read_ppm(options.color.input);
  const Lut3D lut = load_cube(options.color.lut);
  const GradeSettings settings = make_settings(options.color);
  const GradingKernel kernel(&lut, settings);

  const RenderOutput scalar = render_scalar(input, kernel);
  TiledOptions tiled;
  tiled.threads = options.color.threads;
  const RenderOutput threaded = render_tiled(input, kernel, tiled);

  CompareReport report;
  report.pixels = scalar.pixel_count();
  report.threshold = options.threshold;
  report.hash_scalar = compute_statistics(scalar).hash;
  report.hash_tiled = compute_statistics(threaded).hash;

  // Per-channel comparison over the final graded floats. The first difference
  // beyond the threshold wins; later ones never overwrite it, so the report is
  // independent of traversal details.
  bool saw_diff = false;
  double max_diff = 0.0;
  for (std::size_t p = 0; p < report.pixels; ++p) {
    for (std::size_t c = 0; c < 3; ++c) {
      const float a = scalar.after[p * 3 + c];
      const float b = threaded.after[p * 3 + c];
      const double diff = std::fabs(static_cast<double>(a) - static_cast<double>(b));
      max_diff = std::max(max_diff, diff);
      if (!saw_diff && diff > options.threshold) {
        saw_diff = true;
        report.has_first_diff = true;
        report.first_x = p % static_cast<std::size_t>(input.width);
        report.first_y = p / static_cast<std::size_t>(input.width);
        report.first_channel = static_cast<int>(c);
        report.left = a;
        report.right = b;
      }
    }
  }
  report.max_diff = max_diff;
  report.match = !saw_diff;

  print_compare_report(report);
  return static_cast<int>(report.match ? ExitCode::kOk : ExitCode::kMismatch);
}

}  // namespace lutgrade
