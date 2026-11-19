// Chunked local checkpoint index with atomic replacement and verification.
// Each processing block is written as chunk_XXXXX (via a hidden temp file +
// rename), carrying an input digest, its time range and a CRC. A manifest lists
// completed blocks. Temp files are never treated as valid checkpoints; a
// corrupt chunk rolls the resume position back to the last valid boundary.
#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace media {

struct CheckpointBlock {
  std::uint32_t block_id = 0;
  std::uint64_t input_hash = 0;     // FNV-1a digest of the block's input
  std::int64_t start_us = 0;
  std::int64_t end_us = 0;
  std::uint32_t crc32 = 0;          // over the block payload description
  std::uint64_t packet_count = 0;
};

struct CheckpointState {
  std::uint64_t run_input_hash = 0; // hash of the whole run input
  std::uint32_t codec = 0;
  std::uint32_t total_blocks = 0;
  std::vector<CheckpointBlock> blocks;
};

enum class CheckpointStatus {
  Ok = 0,
  Resumed = 1,          // one or more valid blocks were reused
  RolledBack = 2,       // a corrupt block forced a rollback
  Unrecoverable = 3,    // manifest/root is unreadable
};

class CheckpointIndex {
 public:
  // dir is created if absent; path traversal in the dir name is rejected.
  explicit CheckpointIndex(std::string dir);

  // Persist one block atomically and update the manifest atomically.
  bool write_block(const CheckpointBlock& block, std::string& error);

  // Load + verify every chunk. Returns the blocks that survived validation;
  // corrupt chunks are skipped and the status reflects any rollback.
  CheckpointState load(CheckpointStatus& status) const;

  // Simulate an interrupted process: leave a hidden temp file behind and do
  // NOT publish the block, so the next resume proves tmp is ignored.
  bool simulate_interrupt(const CheckpointBlock& block, std::string& error);

  const std::string& dir() const noexcept { return dir_; }
  std::uint32_t completed_count() const noexcept;

 private:
  std::string dir_;
  std::string manifest_path() const;
  std::string chunk_path(std::uint32_t id) const;
};

// Serialize/parse one chunk's text representation (CRC stored inside).
std::string serialize_block(const CheckpointBlock& b);
bool parse_block(const std::string& text, CheckpointBlock& out,
                 std::string& error);

// Validate an on-disk block against the expected run input hash.
bool verify_block(const CheckpointBlock& b, std::uint64_t run_input_hash,
                  std::string& error);

} // namespace media
