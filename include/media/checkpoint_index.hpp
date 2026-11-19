// 检查点索引：每 N 个包分块落盘（校验和 + 输入摘要）。
// 重启时逐块校验，跳过已提交块；校验失败回退到最近有效块并报告原因。
// 重复运行不会重复计入已提交块。
#pragma once

#include "media/error_model.hpp"
#include "media/packet.hpp"

#include <cstdint>
#include <filesystem>
#include <vector>

namespace media {

// 拒绝输出目录内的路径穿越：name 必须是纯文件名。
bool is_safe_filename(const std::string& name);

struct ChunkMeta {
    uint32_t chunk_id = 0;
    uint64_t first_seq = 0;
    uint64_t last_seq = 0;
    uint64_t packet_count = 0;
    uint64_t payload_bytes = 0;
    uint64_t checksum = 0;     // 已提交包内容的 FNV-1a
    uint64_t input_digest = 0; // 截至本块的滚动输入摘要
};

uint64_t fnv1a64(const void* data, size_t len, uint64_t seed = 0);

class CheckpointIndex {
  public:
    CheckpointIndex(std::filesystem::path dir, uint32_t packets_per_chunk)
        : dir_(std::move(dir)), per_chunk_(packets_per_chunk) {}

    // 加载并校验已有检查点。返回是否成功建立续跑状态；
    // 校验失败时回退到最近有效块并记录原因。
    bool load(ErrorModel& errors);

    // 提交一个块（落盘 chunk 文件）。写失败记录 WriteFailure 并返回 false。
    bool commit_chunk(const std::vector<MediaPacket>& packets,
                      ErrorModel& errors);

    uint64_t resume_sequence() const;      // 下一个待处理序号
    uint64_t committed_packets() const;    // 已提交包总数
    uint64_t committed_bytes() const;      // 已提交载荷字节
    uint32_t chunk_count() const { return static_cast<uint32_t>(chunks_.size()); }
    uint32_t packets_per_chunk() const { return per_chunk_; }
    bool loaded() const { return loaded_; }

  private:
    std::filesystem::path chunk_path(uint32_t id) const;

    std::filesystem::path dir_;
    uint32_t per_chunk_;
    std::vector<ChunkMeta> chunks_;
    uint64_t committed_packets_ = 0;
    uint64_t committed_bytes_ = 0;
    bool loaded_ = false;
};

} // namespace media
