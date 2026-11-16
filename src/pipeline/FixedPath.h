#pragma once
#include "image/FrameIO.h"
#include "pipeline/PipelineOptions.h"

namespace lg {

// Engine fast path: table entries, sample coordinates and outputs are
// quantized to 12 bit. Shares the Kernel object with CpuPath.
class FixedPath {
public:
  FixedPath(const LutTable& table, const Kernel& kernel, PipelineOptions opts);
  Frame process(const Frame& in) const;

private:
  LutTable quant_; // 12-bit quantized copy of the table
  const Kernel* kernel_;
  PipelineOptions opts_;
  TransformSetup tf_;
};

} // namespace lg
