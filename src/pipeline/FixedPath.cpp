#include "pipeline/FixedPath.h"

namespace lg {

FixedPath::FixedPath(const LutTable& table, const Kernel& kernel,
                     PipelineOptions opts)
    : quant_(table), kernel_(&kernel), opts_(opts),
      tf_(makeTransformSetup(opts)) {
  for (auto& v : quant_.data)
    v = static_cast<float>(quantize12(v));
}

Frame FixedPath::process(const Frame& in) const {
  Frame out;
  out.w = in.w;
  out.h = in.h;
  out.px.resize(in.px.size());
  for (std::size_t i = 0; i < in.px.size(); i += 3) {
    double rgb[3] = {in.px[i], in.px[i + 1], in.px[i + 2]};
    if (tf_.needsIn)
      applyTransform(opts_.inCs, opts_.lutCs, tf_.mIn, rgb);
    double q[3];
    for (int c = 0; c < 3; ++c)
      q[c] = quantize12(rgb[c]);
    double sampled[3];
    kernel_->sample(quant_, q[0], q[1], q[2], sampled);
    for (int c = 0; c < 3; ++c)
      sampled[c] = quantize12(sampled[c]);
    if (tf_.needsOut)
      applyTransform(opts_.lutCs, opts_.outCs, tf_.mOut, sampled);
    for (int c = 0; c < 3; ++c)
      out.px[i + c] = static_cast<float>(sampled[c]);
  }
  return out;
}

} // namespace lg
