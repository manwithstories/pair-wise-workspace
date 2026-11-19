// checkpoint.hpp — 绑定输入摘要/参数版本/阶段游标的检查点
#pragma once
#include "common.hpp"
#include <vector>
#include <optional>

namespace md {

// 流水线阶段（游标单调推进）
enum class PipelineStage : uint8_t {
    Source = 0, Timeline = 1, Segment = 2, Silence = 3,
    Fingerprint = 4, Complete = 5,
};
const char* stage_name(PipelineStage s) noexcept;

struct SegmentCheckpoint {
    uint32_t id = 0;
    std::string sha256_hex;       // 落盘片段哈希（重启后校验）
    int64_t begin_us = 0;
    int64_t end_us = 0;
    uint32_t packets = 0;
};

struct CheckpointData {
    uint32_t param_version = PARAM_VERSION;
    std::array<uint8_t, 32> input_sha256{};
    // 参数指纹（覆盖所有影响输出的参数）
    std::array<uint8_t, 32> params_sha256{};
    PipelineStage stage = PipelineStage::Source;
    uint32_t source_cursor = 0;       // 已消费记录数
    uint64_t timeline_cursor = 0;     // 已交付扩展包数（全部 SSRC 合计）
    std::vector<SegmentCheckpoint> segments;
    std::string recovered_stage = "none";
};

// 由配置计算参数指纹
std::array<uint8_t, 32> hash_config(const ProcessConfig& cfg);

// 原子写检查点
bool save_checkpoint(const std::string& path, const CheckpointData& cp,
                     PipelineError& err);

// 读取检查点；文件不存在返回 nullopt（首次运行）
std::optional<CheckpointData> load_checkpoint(const std::string& path,
                                              PipelineError& err);

// 兼容性校验：输入摘要与参数版本/指纹必须一致，否则返回拒绝原因
enum class CpReject { None, Missing, ParamVersion, InputChanged, ParamsChanged, Corrupt };
CpReject validate_checkpoint(const CheckpointData& cp,
                             const std::array<uint8_t, 32>& input_sha,
                             const std::array<uint8_t, 32>& params_sha);
const char* cp_reject_reason(CpReject r) noexcept;

} // namespace md
