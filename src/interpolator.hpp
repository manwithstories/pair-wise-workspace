#pragma once
#include <array>
#include <cstddef>
#include <cstdint>
#include <vector>

#include "color_matrix.hpp"
#include "error.hpp"
#include "image_io.hpp"
#include "lut_parser.hpp"

// The shared grading kernel.
//
// Both the scalar and the tiled renderer call exactly one function,
// GradingKernel::process(), for every pixel. Neither path reimplements the
// transform, so the only possible difference between them is scheduling.
//
// Per-pixel pipeline (fixed order, documented in run's stage log):
//   1. normalise the 8-bit sample to 0..1
//   2. decode the source transfer function to linear light
//   3. apply the 3x3 primary conversion matrix in linear light
//   4. clamp negatives / >1 and record whether any channel clipped
//   5. encode with the destination transfer function
//   6. sample the 3D LUT with trilinear interpolation (domain-mapped,
//      boundary-clamped) in the destination-encoded domain
//   7. clamp the LUT result and detect non-finite values
//
// NaN / infinity anywhere in steps 1-7 sets PixelResult::non_finite, which the
// renderers turn into a render failure naming the offending pixel. Nothing
// silently propagates a NaN into statistics.
namespace lutgrade {

// Trilinear sample of `lut` at `encoded` (already in the destination encoding).
// The input is mapped through DOMAIN_MIN/MAX, clamped to the cube boundary and
// interpolated in file order (red fastest). Throws Error(kRender) if the LUT is
// malformed at this point or the input is not finite.
std::array<float, 3> lut_sample_trilinear(const Lut3D& lut,
                                          const std::array<double, 3>& encoded);

struct PixelResult {
  std::array<float, 3> rgb{};       // final graded value, 0..1
  std::array<float, 3> pre_lut{};   // value entering the LUT, 0..1
  bool clipped = false;             // any channel clipped in step 4 or 7
  bool non_finite = false;          // NaN/inf detected anywhere
};

struct GradeSettings {
  ColorSpace in_space = ColorSpace::kSrgb;
  ColorSpace out_space = ColorSpace::kSrgb;
  Matrix3 matrix = Matrix3::identity();
};

class GradingKernel {
 public:
  GradingKernel(const Lut3D* lut, GradeSettings settings);

  // Process one 8-bit RGB pixel. `x`,`y` are used only in diagnostics.
  PixelResult process(std::uint8_t r, std::uint8_t g, std::uint8_t b,
                      int x, int y) const;

  const GradeSettings& settings() const noexcept { return settings_; }
  const Lut3D* lut() const noexcept { return lut_; }

 private:
  const Lut3D* lut_;
  GradeSettings settings_;
};

// Output of a render pass: float images, same dimensions as the input.
// `before` holds the pixel entering the LUT, `after` the graded result. Both are
// kept so statistics can report the grading delta without recomputing.
struct RenderOutput {
  int width = 0;
  int height = 0;
  std::vector<float> before;  // 3 per pixel, interleaved RGB
  std::vector<float> after;   // 3 per pixel, interleaved RGB
  std::uint64_t clipped_pixels = 0;

  std::size_t pixel_count() const noexcept {
    return static_cast<std::size_t>(width) * static_cast<std::size_t>(height);
  }
};

// Converts a float image back to 8-bit, rounding half away from zero.
Image quantize(const RenderOutput& rendered);

}  // namespace lutgrade
