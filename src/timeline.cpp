#include "timeline.hpp"
// 批次 3 实现
namespace md {
void TimelineNormalizer::push(MediaPacket, std::vector<MediaPacket>&,
                              std::vector<PipelineError>&) {}
void TimelineNormalizer::flush(std::vector<MediaPacket>&,
                               std::vector<PipelineError>&) {}
void TimelineNormalizer::apply_rtcp(const RtcpReport&,
                                    std::vector<PipelineError>&) {}
} // namespace md
