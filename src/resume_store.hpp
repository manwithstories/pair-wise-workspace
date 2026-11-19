// resume_store.hpp -- atomic, self-validating checkpoints.
//
// A checkpoint is written after each packet batch and records the stage the
// job reached, the input checksum, the config digest and the offsets already
// committed.  On resume the store verifies all three; any mismatch refuses
// the resume with the reason instead of silently starting over or, worse,
// appending to an output produced from different input.
#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "media_error.hpp"
#include "timeline.hpp"

namespace mediaqc {

enum class JobStage {
  Started,
  Scanned,
  Streaming,
  Written,
  Verified,
  Failed,
};

const char* job_stage_name(JobStage s);
bool job_stage_from_name(const std::string& name, JobStage& out);

// Resume is only allowed when the stage is Streaming or later and the output
// it refers to still matches.
struct Checkpoint {
  int version = 1;
  JobStage stage = JobStage::Started;
  std::string input_path;
  std::string input_sha256;
  int64_t input_size = 0;
  std::string config_digest;
  std::string output_path;

  // Bytes of the source consumed and packets emitted so far.
  int64_t input_offset = 0;
  int64_t packets_written = 0;
  int64_t bytes_written = 0;
  // Per-stream timeline state, indexed by demuxer stream index.
  std::vector<TimelineState> per_stream;
  std::vector<int> stream_indices;
  // Repair/error counters so the resumed run can report a whole-asset total.
  int64_t repairs = 0;
  int64_t errors = 0;
  int64_t dropped = 0;
  std::string output_sha256;  // set once Written
  std::string written_at;     // ISO-8601, informational only

  bool usable() const {
    return version == 1 && !input_sha256.empty() && !config_digest.empty();
  }
};

struct ResumeDecision {
  bool resume = false;
  std::string reason;  // why resume was refused, empty when granted
  Checkpoint checkpoint;
  int64_t seconds_saved = 0;  // estimated from committed bytes
};

const char* checkpoint_refusal_code(Code c);

class ResumeStore {
 public:
  // Directory holding checkpoints; one file per input key.
  explicit ResumeStore(std::filesystem::path dir);

  std::filesystem::path checkpoint_path(const std::string& input_path) const;

  // Writes `cp` atomically (temp file + fsync + rename).
  bool save(const Checkpoint& cp, std::string& err);

  // Loads and validates.  Returns false only on an unreadable/corrupt file;
  // policy mismatches are reported via `decision`.
  bool load_for(const std::string& input_path, const std::string& input_sha256,
                const std::string& config_digest, const std::string& output_path,
                ResumeDecision& decision, Error& err);

  bool remove(const std::string& input_path);

 private:
  std::filesystem::path dir_;
};

// Serialises a checkpoint to a stable single-line key=value format.  Field
// order is fixed so the same job always produces byte-identical checkpoints.
std::string serialize_checkpoint(const Checkpoint& cp);
bool parse_checkpoint(const std::string& text, Checkpoint& cp, std::string& err);

// Compact digest of everything that must match for a resume to be safe.
std::string config_digest(const std::string& config_summary);

}  // namespace mediaqc
