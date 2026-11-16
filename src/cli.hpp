#pragma once
#include <string>

#include "color_matrix.hpp"
#include "error.hpp"

// Argument validation that CLI11 does not express declaratively.
//
// CLI11 handles presence, types and help. Everything that needs a semantic
// check — supported space names, thread bounds, path requirements, grid sizes —
// lives here so every subcommand validates identically and returns kUsage with
// a message that names the offending flag.
//
// The structs carry both the raw CLI text and the parsed values; the validate_*
// functions fill the parsed fields, so command code never re-parses.
namespace lutgrade {

inline constexpr int kMaxThreads = 64;

struct ColorOptions {
  std::string input;
  std::string lut;
  std::string in_space_text;
  std::string out_space_text;
  int threads = 1;

  ColorSpace in_space = ColorSpace::kSrgb;
  ColorSpace out_space = ColorSpace::kSrgb;
};

struct RenderOptions {
  ColorOptions color;
  std::string output;
  std::string path = "scalar";  // "scalar" | "tiled"
  bool tiled = false;
};

void validate_render(RenderOptions& options);

struct FixtureOptions {
  std::string dir;
  int size = 33;
};

void validate_fixture(const FixtureOptions& options);

struct CompareOptions {
  ColorOptions color;
  double threshold = 1.0e-6;  // max abs per-channel difference allowed
};

void validate_compare(CompareOptions& options);

}  // namespace lutgrade
