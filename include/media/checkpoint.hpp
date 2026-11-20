#pragma once
// Batch resume: per-track checkpoints validated against source size, mtime and
// a parameter digest. Changed inputs invalidate the checkpoint with a reason.
#include <cstdint>
#include <string>
#include <vector>

namespace mediaqc {

struct CheckpointEntry {
    std::string track_id;
    std::string source_path;
    int64_t source_size = 0;
    int64_t source_mtime_ns = 0;
    std::string param_digest;     // hash of all processing parameters
    std::string output_path;
    std::string output_relpath;   // relative to batch output dir
    int64_t output_bytes = 0;
    bool completed = false;
};

struct Invalidation {
    std::string track_id;
    std::string reason;
};

class CheckpointStore {
public:
    explicit CheckpointStore(std::string path);

    // Loads the store if present; missing file means a fresh batch.
    void load();
    // Atomic write of the current store.
    void save() const;

    // Returns the entry if it still matches the current source and parameters;
    // otherwise records an invalidation, drops the entry and returns nullptr.
    const CheckpointEntry* valid_entry(const std::string& track_id, int64_t size,
                                       int64_t mtime_ns, const std::string& digest);

    void upsert(CheckpointEntry e);
    const std::vector<CheckpointEntry>& entries() const { return entries_; }
    const std::vector<Invalidation>& invalidations() const { return invalidations_; }

    // FNV-1a 64 over the parameter string, returned as hex.
    static std::string digest_params(const std::string& params);

private:
    std::string path_;
    std::vector<CheckpointEntry> entries_;
    std::vector<Invalidation> invalidations_;
};

}  // namespace mediaqc
