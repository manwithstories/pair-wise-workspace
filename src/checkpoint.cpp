#include "media/checkpoint.hpp"

#include <fstream>

#include "media/fs_util.hpp"
#include "media/json.hpp"

namespace mediaqc {

CheckpointStore::CheckpointStore(std::string path) : path_(std::move(path)) {}

std::string CheckpointStore::digest_params(const std::string& params) {
    uint64_t h = 0xcbf29ce484222325ULL;
    for (unsigned char c : params) {
        h ^= c;
        h *= 0x100000001b3ULL;
    }
    char buf[17];
    std::snprintf(buf, sizeof(buf), "%016llx", static_cast<unsigned long long>(h));
    return buf;
}

void CheckpointStore::load() {
    const auto st = fs::stat(path_);
    if (!st.exists) return;
    json::Value doc;
    try {
        doc = json::parse(fs::read_file(path_));
    } catch (const std::exception&) {
        invalidations_.push_back({"*", "checkpoint unreadable; starting fresh"});
        entries_.clear();
        return;
    }
    const auto* arr = doc.find("entries");
    if (!arr) return;
    for (const auto& je : arr->as_array()) {
        CheckpointEntry e;
        e.track_id = je.find("track_id")->as_string();
        e.source_path = je.find("source_path")->as_string();
        e.source_size = je.find("source_size")->as_int();
        e.source_mtime_ns = je.find("source_mtime_ns")->as_int();
        e.param_digest = je.find("param_digest")->as_string();
        e.output_path = je.find("output_path")->as_string();
        e.output_relpath = je.find("output_relpath")->as_string();
        e.output_bytes = je.find("output_bytes")->as_int();
        e.completed = je.find("completed")->as_bool();
        entries_.push_back(std::move(e));
    }
}

void CheckpointStore::save() const {
    json::Array arr;
    for (const auto& e : entries_) {
        arr.emplace_back(json::Object{
            {"track_id", e.track_id},
            {"source_path", e.source_path},
            {"source_size", e.source_size},
            {"source_mtime_ns", e.source_mtime_ns},
            {"param_digest", e.param_digest},
            {"output_path", e.output_path},
            {"output_relpath", e.output_relpath},
            {"output_bytes", e.output_bytes},
            {"completed", e.completed},
        });
    }
    json::Value root(json::Object{{"schema_version", 1}, {"entries", json::Value(std::move(arr))}});
    fs::write_atomic(path_, json::dump(root));
}

const CheckpointEntry* CheckpointStore::valid_entry(const std::string& track_id, int64_t size,
                                                    int64_t mtime_ns,
                                                    const std::string& digest) {
    for (auto it = entries_.begin(); it != entries_.end(); ++it) {
        if (it->track_id != track_id || !it->completed) continue;
        if (it->source_size != size) {
            invalidations_.push_back({track_id, "source size changed; checkpoint discarded"});
            entries_.erase(it);
            return nullptr;
        }
        if (it->source_mtime_ns != mtime_ns) {
            invalidations_.push_back({track_id, "source mtime changed; checkpoint discarded"});
            entries_.erase(it);
            return nullptr;
        }
        if (it->param_digest != digest) {
            invalidations_.push_back({track_id, "processing parameters changed; checkpoint discarded"});
            entries_.erase(it);
            return nullptr;
        }
        if (!fs::stat(it->output_path).exists) {
            invalidations_.push_back({track_id, "output missing; checkpoint discarded"});
            entries_.erase(it);
            return nullptr;
        }
        return &*it;
    }
    return nullptr;
}

void CheckpointStore::upsert(CheckpointEntry e) {
    for (auto& x : entries_)
        if (x.track_id == e.track_id) { x = std::move(e); return; }
    entries_.push_back(std::move(e));
}

}  // namespace mediaqc
