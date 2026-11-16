#pragma once
#include <string>

namespace lg {

struct Mat3 {
  double a[9]; // row-major
};

Mat3 matMul(const Mat3& x, const Mat3& y);
double matDet(const Mat3& m);
// Throws InternalError when the matrix is singular.
Mat3 matInverse(const Mat3& m, const char* what);

enum class ColorSpaceId { Rec709, Rec2020, SRgb, ACEScg };

// Accepts rec709 | rec2020 | srgb | acescg (case-insensitive).
ColorSpaceId parseColorSpace(const std::string& name);
const char* colorSpaceName(ColorSpaceId id);

double transferDecode(ColorSpaceId id, double v); // encoded -> linear
double transferEncode(ColorSpaceId id, double v); // linear -> encoded

const Mat3& rgbToXyz(ColorSpaceId id);
const Mat3& xyzToRgb(ColorSpaceId id);

// Full encoded->encoded conversion between two spaces.
void convertPixel(ColorSpaceId from, ColorSpaceId to, const double in[3],
                  double out[3]);

} // namespace lg
