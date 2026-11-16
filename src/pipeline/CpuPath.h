#pragma once
#include "image/FrameIO.h"
#include "pipeline/PipelineOptions.h"

namespace lg {

// Full-float offline render path.
class CpuPath {
public:
  CpuPath(const LutTable& table, const Kernel& kernel, PipelineOptions opts);
  Frame process(const Frame& in) const;

private:
  const LutTable* table_;
  const Kernel* kernel_;
  PipelineOptions opts_;
  TransformSetup tf_;
};

} // namespace lg
