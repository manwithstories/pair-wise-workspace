#pragma once
#include "lut/LutTable.h"

namespace lg {

enum class Interp { Trilinear, Tetrahedral };
enum class Edge { Clamp, Extend };

// Stateless sampling kernel shared by the float and fixed-point paths.
// Weights are accumulated in double.
class Kernel {
public:
  explicit Kernel(Interp interp = Interp::Tetrahedral, Edge edge = Edge::Clamp)
      : interp_(interp), edge_(edge) {}

  void sample(const LutTable& t, double r, double g, double b, double out[3]) const;

private:
  Interp interp_;
  Edge edge_;
};

} // namespace lg
