#pragma once
// Chunked, checksummed checkpoint store for the keyframe index. Index
// records are appended in fixed-size blocks; every block is flushed through
// a temp file and atomically renamed so a crash never leaves a half-written
// block. On open, the last valid block is kept and any corrupt tail block
// is rolled back, letting recovery resume deterministically.

#include <cstdint>
#include <string>
#include <vector>

#include "media_archive/error.h"
#include "media_archive/keyframe_index.h"
#include "media_archive/types.h"

namespace media_archive {

struct CheckpointConfig {
  uint32_t records_per_block = 64;
  uint32_t max_records_per_block = kMaxKeyframesPerBlock;
};

struct CheckpointOpenResult {
  std::vector<KeyframeEntry> recovered;  // entries from valid blocks
  uint64_t last_valid_block = UINT64_MAX; // UINT64_MAX == none
  uint64_t rolled_back_block = UINT64_MAX;
  bool resumed = false;
};

class CheckpointStore {
 public:
  CheckpointStore() = default;
  ~CheckpointStore();
  CheckpointStore(const CheckpointStore&) = delete;
  CheckpointStore& operator=(const CheckpointStore&) = delete;

  // Open (or create) the checkpoint directory and recover valid blocks.
  Diagnostic open(const std::string& dir, const CheckpointConfig& config);

  // Append an entry; flushes a block when the configured size is reached.
  // `crash_after_appends` simulates a crash: after that many total appends
  // the process aborts without finalizing the in-flight block.
  Diagnostic append(const KeyframeEntry& entry,
                    uint64_t crash_after_appends = UINT64_MAX);

  // Force-flush the current partial block and write the manifest.
  Diagnostic flush();

  uint64_t total_appends() const { return total_appends_; }
  uint64_t blocks_written() const { return next_block_; }

 private:
  std::string dir_;
  CheckpointConfig cfg_{};
  std::vector<KeyframeEntry> pending_;
  uint64_t next_block_ = 0;
  uint64_t total_appends_ = 0;
  bool open_ = false;
  bool crashed_ = false;

  std::string block_path(uint64_t block) const;
  std::string temp_path(uint64_t block) const;
  std::string manifest_path() const;
  Diagnostic write_block(uint64_t block,
                         const std::vector<KeyframeEntry>& entries);
};

// Scan a checkpoint directory and recover every valid block. A corrupt tail
// block is reported via rolled_back_block rather than failing the whole run.
Diagnostic open_checkpoint_dir(const std::string& dir,
                               const CheckpointConfig& config,
                               CheckpointOpenResult* result);

}  // namespace media_archive
