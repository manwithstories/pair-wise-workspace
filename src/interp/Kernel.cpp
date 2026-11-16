#include "interp/Kernel.h"
#include "Errors.h"
#include <algorithm>
#include <cmath>

namespace lg {

void Kernel::sample(const LutTable& t, double r, double g, double b,
                    double out[3]) const {
  const int n = t.size;
  const double in[3] = {r, g, b};
  int base[3];
  double frac[3];
  for (int c = 0; c < 3; ++c) {
    double w = t.dmax[c] - t.dmin[c];
    if (w <= 0.0)
      throw InternalError("zero-width LUT domain on channel " + std::to_string(c));
    double p = (in[c] - t.dmin[c]) / w * (n - 1);
    if (edge_ == Edge::Clamp)
      p = p < 0.0 ? 0.0 : (p > n - 1 ? n - 1 : p);
    int i = static_cast<int>(std::floor(p));
    i = std::max(0, std::min(i, n - 2)); // Extend: keep frac outside [0,1]
    base[c] = i;
    frac[c] = p - i;
  }

  double acc[3] = {0.0, 0.0, 0.0};
  if (interp_ == Interp::Trilinear) {
    for (int db = 0; db < 2; ++db)
      for (int dg = 0; dg < 2; ++dg)
        for (int dr = 0; dr < 2; ++dr) {
          double w = (dr ? frac[0] : 1.0 - frac[0]) *
                     (dg ? frac[1] : 1.0 - frac[1]) *
                     (db ? frac[2] : 1.0 - frac[2]);
          if (w == 0.0)
            continue;
          const float* nd = t.node(base[0] + dr, base[1] + dg, base[2] + db);
          for (int c = 0; c < 3; ++c)
            acc[c] += w * nd[c];
        }
  } else {
    const double fx = frac[0], fy = frac[1], fz = frac[2];
    int v1[3], v2[3];
    double w1, w2;
    if (fx >= fy) {
      if (fy >= fz)      { w1 = fx - fy; v1[0]=1; v1[1]=0; v1[2]=0;
                           w2 = fy - fz; v2[0]=1; v2[1]=1; v2[2]=0; }
      else if (fx >= fz) { w1 = fx - fz; v1[0]=1; v1[1]=0; v1[2]=0;
                           w2 = fz - fy; v2[0]=1; v2[1]=0; v2[2]=1; }
      else               { w1 = fz - fx; v1[0]=0; v1[1]=0; v1[2]=1;
                           w2 = fx - fy; v2[0]=1; v2[1]=0; v2[2]=1; }
    } else {
      if (fz >= fy)      { w1 = fz - fy; v1[0]=0; v1[1]=0; v1[2]=1;
                           w2 = fy - fx; v2[0]=0; v2[1]=1; v2[2]=1; }
      else if (fx >= fz) { w1 = fy - fx; v1[0]=0; v1[1]=1; v1[2]=0;
                           w2 = fx - fz; v2[0]=1; v2[1]=1; v2[2]=0; }
      else               { w1 = fy - fz; v1[0]=0; v1[1]=1; v1[2]=0;
                           w2 = fz - fx; v2[0]=0; v2[1]=1; v2[2]=1; }
    }
    const double w0 = 1.0 - std::max({fx, fy, fz});
    const double w3 = std::min({fx, fy, fz});
    const int offs[4][3] = {{0, 0, 0}, {v1[0], v1[1], v1[2]},
                            {v2[0], v2[1], v2[2]}, {1, 1, 1}};
    const double ws[4] = {w0, w1, w2, w3};
    for (int k = 0; k < 4; ++k) {
      if (ws[k] == 0.0)
        continue;
      const float* nd = t.node(base[0] + offs[k][0], base[1] + offs[k][1],
                               base[2] + offs[k][2]);
      for (int c = 0; c < 3; ++c)
        acc[c] += ws[k] * nd[c];
    }
  }
  for (int c = 0; c < 3; ++c)
    out[c] = acc[c];
}

} // namespace lg
