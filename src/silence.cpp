#include "silence.hpp"
// 批次 5 实现
namespace md {
struct SilenceDetector::Impl {};
SilenceDetector::SilenceDetector(const ProcessConfig& cfg) : cfg_(cfg), impl_(std::make_unique<Impl>()) {}
SilenceDetector::~SilenceDetector() = default;
void SilenceDetector::push(const MediaPacket&, std::vector<PipelineError>&) {}
std::vector<SilenceInterval> SilenceDetector::finish() { return {}; }
} // namespace md
