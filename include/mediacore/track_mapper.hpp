// mediacore/track_mapper.hpp — 输入声道 -> 命名输出轨道映射
//
// 配置驱动的去交错映射：每个输出轨道声明占用的输入声道编号集合。
// 校验规则（全部在 applyConfig 阶段一次性给出可诊断错误）：
//   1. 输入声道编号越界 -> MappingOutOfRange（不可恢复，拒绝该文件）
//   2. 同一输入声道被两个轨道重复占用 -> MappingDuplicate
//   3. 轨道声道数与布局要求不兼容 -> MappingIncompatible
//      （单声道轨道恰好 1 个输入声道；立体声轨道恰好 2 个且顺序固定 L,R）
#pragma once

#include <string>
#include <vector>

#include "mediacore/error.hpp"
#include "mediacore/pcm.hpp"

namespace mediacore {

enum class TrackLayout {
    Mono,      // 单声道
    Stereo,    // 立体声（输入声道顺序即 L,R）
};

const char* trackLayoutName(TrackLayout layout) noexcept;

struct OutputTrack {
    std::string name;
    TrackLayout layout = TrackLayout::Mono;
    std::vector<int> inputChannels; // 占用的输入声道编号
};

struct MappingConfig {
    std::vector<OutputTrack> tracks;
};

// 单条输出轨道的一个数据块（已去交错，PCM 帧序与输入一致）
struct MappedBlock {
    std::string trackName;
    TrackLayout layout = TrackLayout::Mono;
    PcmFormat format;            // 声道数已更新为轨道声道数
    SampleTick startSample = 0;
    std::vector<std::uint8_t> interleavedPcm;
    BlockKind kind = BlockKind::Normal;
};

class TrackMapper {
public:
    // 校验并装载映射配置；返回首个违例错误（含错误码与可恢复标志=false）
    Result<void> applyConfig(const MappingConfig& config,
                             const PcmFormat& inputFormat);

    const std::vector<OutputTrack>& tracks() const noexcept { return tracks_; }
    const PcmFormat& inputFormat() const noexcept { return inputFormat_; }

    // 将一个输入块去交错为每轨道一个 MappedBlock。
    // Corrupt/Gap/Duplicate 块原样传播分类，Corrupt/Gap 不携带 PCM。
    std::vector<MappedBlock> mapBlock(const AudioBlock& in) const;

private:
    MappingConfig config_;
    std::vector<OutputTrack> tracks_;
    PcmFormat inputFormat_;
};

} // namespace mediacore
