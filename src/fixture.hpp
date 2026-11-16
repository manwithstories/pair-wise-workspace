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

// Amplitude of the S-curve. Chosen so the demo grade's own deviation (pre-LUT
// vs post-LUT) lands in the 1e-5..1e-4 band the acceptance criteria describe
// at 33^3 — a gentle, realistic tone curve rather than a pixel-shifting one.
inline constexpr double kDemoCurveAmplitude = 0.0008;

Image make_fixture_image();
std::string make_demo_cube(int grid_size, const std::string& title);

// Writes input.ppm, demo.cube, lut17.cube, lut33.cube and lut65.cube into
// `dir` (created if missing). Throws Error(kUsage) on I/O trouble.
void generate_fixtures(const std::string& dir, int grid_size);

}  // namespace lutgrade
