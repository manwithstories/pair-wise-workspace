#include "pipeline/CpuPath.h"

namespace lg {

CpuPath::CpuPath(const LutTable& table, const Kernel& kernel,
                 PipelineOptions opts)
    : table_(&table), kernel_(&kernel), opts_(opts),
      tf_(makeTransformSetup(opts)) {}

Frame CpuPath::process(const Frame& in) const {
  Frame out;
  out.w = in.w;
  out.h = in.h;
  out.px.resize(in.px.size());
  for (std::size_t i = 0; i < in.px.size(); i += 3) {
    double rgb[3] = {in.px[i], in.px[i + 1], in.px[i + 2]};
    if (tf_.needsIn)
      applyTransform(opts_.inCs, opts_.lutCs, tf_.mIn, rgb);
    double sampled[3];
    kernel_->sample(*table_, rgb[0], rgb[1], rgb[2], sampled);
    if (tf_.needsOut)
      applyTransform(opts_.lutCs, opts_.outCs, tf_.mOut, sampled);
    for (int c = 0; c < 3; ++c)
      out.px[i + c] = static_cast<float>(sampled[c]);
  }
  return out;
}

} // namespace lg
