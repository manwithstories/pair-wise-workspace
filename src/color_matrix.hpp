#pragma once
#include <array>
#include <string>

#include "error.hpp"

// Colour-space handling for the grading pipeline.
//
// Two distinct concepts, kept separate on purpose:
//
//   * Matrix3 — a 3x3 primary/white-point conversion between sRGB, Rec.709,
//     Rec.2020 and ACEScg. These are precomputed constants (derived from the
//     primaries + Bradford chromatic adaptation) so render output is
//     bit-reproducible across platforms. sRGB and Rec.709 share primaries and
//     white point, so their cross-matrix is the identity.
//
//   * Transfer — the electro-optical / opto-electronic function used to move
//     between encoded 0..1 code values and linear light. The renderer
//     decodes with the source transfer function, applies the matrix in
//     linear light, then re-encodes with the destination transfer function.
//
// The pipeline stage list is recorded so `run` can print exactly which
// conversions happened.
namespace lutgrade {

enum class ColorSpace { kSrgb, kRec709, kRec2020, kAcescg };

// Parses the CLI spelling (case-insensitive): srgb, rec709, rec2020, acescg.
// Throws Error(kColor, "space_unknown") for anything else.
ColorSpace parse_color_space(const std::string& name);
const char* color_space_name(ColorSpace space);

struct Matrix3 {
  std::array<double, 9> m{};  // row-major

  static Matrix3 identity();
  // Throws Error(kColor, "matrix_invalid") when `m` is not 3x3-finite.
  static Matrix3 from_row_major(std::array<double, 9> values);
};

Matrix3 multiply(const Matrix3& a, const Matrix3& b);
std::array<double, 3> apply(const Matrix3& m, const std::array<double, 3>& rgb);

// Precomputed conversion between two spaces (identity when equal).
Matrix3 conversion_matrix(ColorSpace from, ColorSpace to);

// Clamps a linear-light triple; reports whether any channel went negative or
// above 1.0 before clamping. Out-of-gamut values are expected when converting
// wide gamut to narrow, so the caller records rather than rejects them.
struct ClampResult {
  std::array<double, 3> rgb{};
  bool clipped_low = false;
  bool clipped_high = false;
  bool non_finite = false;
};
ClampResult clamp_linear(const std::array<double, 3>& linear);

// Transfer functions. `encoded` and `linear` are in 0..1 nominal range;
// values slightly outside are accepted (matrices can push a channel out of
// gamut) but the decoder saturates its input to [0,1] first.
double decode_transfer(ColorSpace space, double encoded);
double encode_transfer(ColorSpace space, double linear);

// Human-readable name of each stage for the conversion log.
const char* transfer_name(ColorSpace space);

}  // namespace lutgrade
