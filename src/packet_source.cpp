#include "packet_source.hpp"
// 批次 2 实现
namespace md {
uint32_t generate_capture(const std::string&, const GenerateOptions&,
                          PipelineError& err) { err = {}; return 0; }
bool PacketSource::open(const std::string&, PipelineError&) { return false; }
bool PacketSource::next_rtp(MediaPacket&, PipelineError&) { return false; }
} // namespace md
