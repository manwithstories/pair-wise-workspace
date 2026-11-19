#include "checkpoint.hpp"
// 批次 7 实现
namespace md {
const char* stage_name(PipelineStage s) noexcept {
    static const char* names[] = {"source","timeline","segment","silence",
                                  "fingerprint","complete"};
    return names[uint8_t(s)];
}
std::array<uint8_t, 32> hash_config(const ProcessConfig&) { return {}; }
bool save_checkpoint(const std::string&, const CheckpointData&, PipelineError&) {
    return false;
}
std::optional<CheckpointData> load_checkpoint(const std::string&, PipelineError&) {
    return std::nullopt;
}
CpReject validate_checkpoint(const CheckpointData&,
                             const std::array<uint8_t, 32>&,
                             const std::array<uint8_t, 32>&) { return CpReject::None; }
const char* cp_reject_reason(CpReject r) noexcept {
    switch (r) {
        case CpReject::None: return "ok";
        case CpReject::Missing: return "checkpoint missing";
        case CpReject::ParamVersion: return "parameter version mismatch";
        case CpReject::InputChanged: return "input summary mismatch";
        case CpReject::ParamsChanged: return "parameters changed";
        case CpReject::Corrupt: return "checkpoint corrupt";
    }
    return "unknown";
}
} // namespace md
