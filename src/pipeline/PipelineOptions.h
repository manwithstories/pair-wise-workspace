#pragma once
#include <cmath>
#include "color/ColorSpace.h"
#include "interp/Kernel.h"

namespace lg {

struct PipelineOptions {
  Interp interp = Interp::Tetrahedral;
  Edge edge = Edge::Clamp;
  ColorSpaceId inCs = ColorSpaceId::Rec709;
  ColorSpaceId lutCs = ColorSpaceId::Rec709;
  ColorSpaceId outCs = ColorSpaceId::Rec709;
  bool inputTransform = true;
  bool outputTransform = true;
};

// Precomputed color transform pair shared by both render paths.
struct TransformSetup {
  Mat3 mIn{};
  Mat3 mOut{};
  bool needsIn = false;
  bool needsOut = false;
};

inline TransformSetup makeTransformSetup(const PipelineOptions& o) {
  TransformSetup s;
  s.needsIn = o.inputTransform && o.inCs != o.lutCs;
  s.needsOut = o.outputTransform && o.outCs != o.lutCs;
  if (s.needsIn)
    s.mIn = matMul(xyzToRgb(o.lutCs), rgbToXyz(o.inCs));
  if (s.needsOut)
    s.mOut = matMul(xyzToRgb(o.outCs), rgbToXyz(o.lutCs));
  return s;
}

inline void applyTransform(ColorSpaceId from, ColorSpaceId to, const Mat3& m,
                           double rgb[3]) {
  double lin[3];
  for (int c = 0; c < 3; ++c)
    lin[c] = transferDecode(from, rgb[c]);
  double v[3];
  for (int i = 0; i < 3; ++i)
    v[i] = m.a[i * 3] * lin[0] + m.a[i * 3 + 1] * lin[1] + m.a[i * 3 + 2] * lin[2];
  for (int c = 0; c < 3; ++c)
    rgb[c] = transferEncode(to, v[c]);
}

inline double quantize12(double v) {
  v = v < 0.0 ? 0.0 : (v > 1.0 ? 1.0 : v);
  return std::round(v * 4095.0) / 4095.0;
}

} // namespace lg
