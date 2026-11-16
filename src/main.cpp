#include <CLI/CLI.hpp>
#include <exception>
#include <iostream>
#include <string>

#include "cli.hpp"
#include "commands.hpp"
#include "error.hpp"

namespace {

// Converts an Error into its exit code, printing the kind and message to
// stderr. Keeps main() free of per-command error handling.
int report_error(const lutgrade::Error& error) {
  std::cerr << "lutgrade: error [" << error.kind() << "]: " << error.message()
            << "\n";
  return static_cast<int>(error.code());
}

}  // namespace

int main(int argc, char** argv) {
  CLI::App app{"lutgrade — offline 3D LUT validation and rendering", "lutgrade"};
  app.require_subcommand(1);

  // ---- generate-fixture -----------------------------------------------------
  lutgrade::FixtureOptions fixture;
  auto* gen = app.add_subcommand("generate-fixture",
                                 "Generate deterministic PPM and .cube fixtures");
  gen->add_option("--dir", fixture.dir, "Output directory")->required();
  gen->add_option("--size", fixture.size, "Demo LUT grid size (17/33/65)")
      ->default_val(33);
  gen->callback([&] { lutgrade::validate_fixture(fixture); });

  // ---- run ------------------------------------------------------------------
  lutgrade::RenderOptions render;
  auto* run = app.add_subcommand("run", "Render a PPM through a 3D LUT");
  run->add_option("--input", render.color.input, "Input PPM")->required();
  run->add_option("--lut", render.color.lut, "Input .cube LUT")->required();
  run->add_option("--in-space", render.color.in_space_text, "Source space")
      ->default_val("srgb");
  run->add_option("--out-space", render.color.out_space_text,
                  "Destination space")
      ->default_val("srgb");
  run->add_option("--path", render.path, "Render path: scalar or tiled")
      ->default_val("scalar");
  run->add_option("--threads", render.color.threads, "Worker count (tiled only)")
      ->default_val(1);
  run->add_option("--output", render.output, "Output PPM")->required();
  run->callback([&] { lutgrade::validate_render(render); });

  // ---- compare --------------------------------------------------------------
  lutgrade::CompareOptions compare;
  auto* cmp = app.add_subcommand("compare",
                                 "Compare scalar and tiled render paths");
  cmp->add_option("--input", compare.color.input, "Input PPM")->required();
  cmp->add_option("--lut", compare.color.lut, "Input .cube LUT")->required();
  cmp->add_option("--in-space", compare.color.in_space_text, "Source space")
      ->default_val("srgb");
  cmp->add_option("--out-space", compare.color.out_space_text,
                  "Destination space")
      ->default_val("srgb");
  cmp->add_option("--threads", compare.color.threads,
                  "Tiled worker count to compare against")
      ->default_val(1);
  cmp->add_option("--threshold", compare.threshold,
                  "Max allowed per-channel difference")
      ->default_val(1.0e-6);
  cmp->callback([&] { lutgrade::validate_compare(compare); });

  try {
    app.parse(argc, argv);
  } catch (const CLI::ParseError& e) {
    // CLI11 returns 0 for --help/--version, 1xx for usage errors; map usage
    // errors onto our kUsage contract.
    const int code = app.exit(e);
    if (code == 0) return 0;
    return static_cast<int>(lutgrade::ExitCode::kUsage);
  }

  try {
    if (gen->parsed()) return lutgrade::execute_fixture(fixture);
    if (run->parsed()) return lutgrade::execute_render(render);
    if (cmp->parsed()) return lutgrade::execute_compare(compare);
  } catch (const lutgrade::Error& e) {
    return report_error(e);
  } catch (const std::exception& e) {
    std::cerr << "lutgrade: unexpected error: " << e.what() << "\n";
    return static_cast<int>(lutgrade::ExitCode::kRender);
  }

  std::cerr << "lutgrade: no subcommand given\n";
  return static_cast<int>(lutgrade::ExitCode::kUsage);
}
