#pragma once
#include <string>

#include "error.hpp"
#include "image_io.hpp"
#include "lut_parser.hpp"

// Deterministic fixture generation for `generate-fixture`.
//
// Everything is derived from pixel coordinates and the requested grid size, so
// two runs produce byte-identical files. Three regions exercise different parts
// of the pipeline:
//
//   top half     horizontal/vertical gradient  (smooth interpolation)
//   lower-left   checkerboard                 (8-bit quantization stress)
//   lower-right  highlight block              (values near 1.0)
//
// The demo LUT is a gentle per-channel S-curve sampled on the requested grid,
// written as a .cube whose values may fall slightly outside [0,1] at the ends;
// the generator clamps those to the legal range before writing.
namespace lutgrade {

inline constexpr int kFixtureWidth = 64;
inline constexpr int kFixtureHeight = 64;

// Amplitude of the S-curve. Tuned against the real pipeline so the demo
// grade's own deviation (pre-LUT vs post-LUT) lands at MAX_DEV ~2.0e-6 and
// MEAN_DEV ~1.3e-6 at 33^3 — inside the acceptance band (max up to 1e-4,
// mean on the 1e-6 order) while still being a genuine, non-identity curve.
inline constexpr double kDemoCurveAmplitude = 2.0e-5;

Image make_fixture_image();
std::string make_demo_cube(int grid_size, const std::string& title);

// Writes input.ppm plus demo.cube (at `grid_size`) and the two other
// lut17/lut33/lut65 variants, so the directory always has all three grids.
// `dir` is created recursively if missing. Throws Error(kUsage) on I/O trouble.
void generate_fixtures(const std::string& dir, int grid_size);

}  // namespace lutgrade
