#pragma once

#include <filesystem>
#include <map>
#include <optional>
#include <string>
#include <vector>

#include "relay/types.hpp"

namespace relay {

enum class StageStatus { kPending, kRunning, kDone, kFailed, kSkipped };

struct StageRecord {
  std::string name;
  std::string input_digest;
  std::string output_digest;
  StageStatus status = StageStatus::kPending;
  std::string payload;  ///< opaque, stage-owned output (e.g. manifest JSON)
  std::string error;
};

struct StageOutcome {
  bool reused = false;      ///< checkpoint was valid and reused
  bool rejected = false;    ///< checkpoint existed but input digest changed
  bool satisfied = false;   ///< result available after the call
  StageRecord record;
};

class CheckpointStore {
 public:
  explicit CheckpointStore(std::filesystem::path directory);

  /// True when a completed record exists for `stage` with the same input
  /// digest.  A differing digest invalidates the record and reports the
  /// failed dependency chain rather than reusing stale output.
  bool can_reuse(const std::string& stage, const std::string& input_digest,
                 StageOutcome& outcome, ErrorSink& errors) const;

  /// Marks the stage as running so an interrupted run is distinguishable
  /// from an untouched one.
  void begin(const std::string& stage, const std::string& input_digest,
             ErrorSink& errors);

  /// Persists a completed record atomically.
  bool complete(const std::string& stage, const std::string& input_digest,
                const std::string& output_digest, const std::string& payload,
                ErrorSink& errors);

  void fail(const std::string& stage, const std::string& input_digest,
            const std::string& reason, ErrorSink& errors);

  [[nodiscard]] std::optional<StageRecord> find(const std::string& stage) const;
  [[nodiscard]] bool loaded() const { return loaded_; }
  [[nodiscard]] const std::string& load_error() const { return load_error_; }
  [[nodiscard]] const std::filesystem::path& path() const { return path_; }

  /// Stages in the order they were completed; used for dependency-chain
  /// reporting and resume summaries.
  [[nodiscard]] std::vector<std::string> completed_order() const;

 private:
  bool load(ErrorSink& errors);
  bool flush(ErrorSink& errors);

  std::filesystem::path path_;
  std::map<std::string, StageRecord> records_;
  std::vector<std::string> order_;
  bool loaded_ = false;
  std::string load_error_;
};

inline constexpr const char* kStageRead = "read";
inline constexpr const char* kStageReorder = "reorder";
inline constexpr const char* kStageTimeline = "timeline";
inline constexpr const char* kStageSegment = "segment";
inline constexpr const char* kStageLadder = "ladder";
inline constexpr const char* kStageManifest = "manifest";

}  // namespace relay
