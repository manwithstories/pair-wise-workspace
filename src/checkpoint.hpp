// 分块检查点与本地索引：原子写块、自校验索引、中断后续跑。
// 索引为带整体 SHA-256 的文本格式；任何截断/损坏/输入摘要变化都拒绝静默复用。
#pragma once
#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

namespace mb {

// 可定位的检查点错误：location 为文件路径、块号或索引行号。
struct CpError {
  std::string code;
  std::string message;
  std::string location;
  explicit operator bool() const { return !code.empty(); }
};

struct BlockRecord {
  int id = 0;
  int64_t start_ticks = 0;
  int64_t end_ticks = 0;
  int64_t frame_count = 0;
  bool complete = false;  // false 表示失败区间，需重跑
  std::string sha256;
  uint64_t size = 0;
};

struct ResumeResult {
  std::vector<int> complete_blocks;  // 已校验可复用的块
  std::vector<int> failed_blocks;    // 标记失败、需重跑的块
  size_t resumed_blocks = 0;
};

struct CheckpointConfig {
  std::filesystem::path work_root;   // 工作目录，检查点固定在其下 checkpoint/
  std::string input_digest;          // 输入摘要（变化即拒绝复用）
  std::string run_id;
};

class CheckpointStore {
 public:
  static std::string compute_digest(const std::string& manifest);

  // 打开或创建存储；已存在索引时立即做结构校验（不校验块，块在 resume 校验）。
  bool open(const CheckpointConfig& cfg, CpError* err);

  // 原子写一个完成块（先 .tmp 再 rename），并落盘更新索引。
  bool write_block(int id, int64_t start, int64_t end, int64_t frames,
                   const std::vector<uint8_t>& payload, CpError* err);
  // 记录失败区间（保留已完成块不动）。
  bool mark_block_failed(int id, int64_t start, int64_t end, CpError* err);

  // 校验索引完整性、输入摘要与每个完成块的大小/SHA，返回可复用块。
  bool resume(ResumeResult* out, CpError* err);

  bool finalize(CpError* err);
  bool finalized() const { return finalized_; }
  const std::vector<BlockRecord>& blocks() const { return blocks_; }
  std::filesystem::path dir() const { return dir_; }
  std::filesystem::path block_path(int id) const;

 private:
  std::filesystem::path dir_, index_path_;
  std::string input_digest_, run_id_;
  std::vector<BlockRecord> blocks_;
  bool finalized_ = false;
  bool flush_index(CpError* err);   // 原子重写索引并附带整体校验和
  bool load_index(CpError* err);    // 解析并校验索引
};

// 稳定错误码（映射到退出码 3）。
inline constexpr const char* kCpIndexCorrupt = "CP_INDEX_CORRUPT";
inline constexpr const char* kCpBlockTruncated = "CP_BLOCK_TRUNCATED";
inline constexpr const char* kCpBlockHash = "CP_BLOCK_HASH_MISMATCH";
inline constexpr const char* kCpInputChanged = "CP_INPUT_CHANGED";
inline constexpr const char* kCpIo = "CP_IO_ERROR";

}  // namespace mb
