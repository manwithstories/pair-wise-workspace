#include "media/checkpoint_index.hpp"

#include <cstdio>
#include <fstream>
#include <sstream>

namespace media {

bool is_safe_filename(const std::string& name) {
    if (name.empty() || name == "." || name == "..") return false;
    return name.find_first_of("/\\") == std::string::npos &&
           name.find("..") == std::string::npos;
}

uint64_t fnv1a64(const void* data, size_t len, uint64_t seed) {
    const auto* p = static_cast<const uint8_t*>(data);
    uint64_t h = seed ? seed : 1469598103934665603ULL;
    for (size_t i = 0; i < len; ++i) {
        h ^= p[i];
        h *= 1099511628211ULL;
    }
    return h;
}

static uint64_t hash_u64(uint64_t h, uint64_t v) {
    return fnv1a64(&v, sizeof(v), h);
}

// 块元数据自校验和（防截断/篡改）。
static uint64_t meta_seal(const ChunkMeta& m) {
    uint64_t h = 0;
    h = hash_u64(h, m.chunk_id);
    h = hash_u64(h, m.first_seq);
    h = hash_u64(h, m.last_seq);
    h = hash_u64(h, m.packet_count);
    h = hash_u64(h, m.payload_bytes);
    h = hash_u64(h, m.checksum);
    h = hash_u64(h, m.input_digest);
    return h ^ 0xC4EC4EC4EC4EC4ECULL;
}

std::filesystem::path CheckpointIndex::chunk_path(uint32_t id) const {
    char name[64];
    std::snprintf(name, sizeof(name), "chunk_%06u.idx", id);
    return dir_ / name;
}

bool CheckpointIndex::load(ErrorModel& errors) {
    chunks_.clear();
    committed_packets_ = 0;
    committed_bytes_ = 0;
    std::error_code ec;
    if (!std::filesystem::exists(dir_, ec)) {
        loaded_ = true;
        return true; // 全新运行
    }
    for (uint32_t id = 0;; ++id) {
        auto path = chunk_path(id);
        if (!std::filesystem::exists(path, ec)) break;
        std::ifstream in(path);
        ChunkMeta m;
        uint64_t seal = 0;
        std::string magic;
        in >> magic >> m.chunk_id >> m.first_seq >> m.last_seq >>
            m.packet_count >> m.payload_bytes >> m.checksum >> m.input_digest >>
            seal;
        bool ok = in && magic == "MBCHK1" && m.chunk_id == id &&
                  seal == meta_seal(m);
        if (!ok) {
            errors.record({ErrorKind::CheckpointCorrupt, "checkpoint",
                           static_cast<int64_t>(id), -1, -1,
                           "chunk file invalid or tampered, rollback to " +
                               std::to_string(id) + " valid chunks"});
            break; // 回退：丢弃 id 及之后的块
        }
        committed_packets_ += m.packet_count;
        committed_bytes_ += m.payload_bytes;
        chunks_.push_back(m);
    }
    loaded_ = true;
    return true;
}

bool CheckpointIndex::commit_chunk(const std::vector<MediaPacket>& packets,
                                   ErrorModel& errors) {
    if (packets.empty()) return true;
    ChunkMeta m;
    m.chunk_id = static_cast<uint32_t>(chunks_.size());
    m.first_seq = packets.front().sequence;
    m.last_seq = packets.back().sequence;
    m.packet_count = packets.size();
    uint64_t h = chunks_.empty() ? 0 : chunks_.back().input_digest;
    for (const auto& p : packets) {
        m.payload_bytes += p.payload_bytes;
        uint64_t ph = hash_u64(p.sequence, p.payload_hash);
        ph = hash_u64(ph, static_cast<uint64_t>(p.pts_ticks));
        m.checksum = fnv1a64(&ph, sizeof(ph), m.checksum);
        h = fnv1a64(&ph, sizeof(ph), h);
    }
    m.input_digest = h;

    std::error_code ec;
    std::filesystem::create_directories(dir_, ec);
    if (ec) {
        errors.record({ErrorKind::WriteFailure, "checkpoint", -1, -1, -1,
                       "cannot create output dir: " + ec.message()});
        return false;
    }
    auto path = chunk_path(m.chunk_id);
    if (!is_safe_filename(path.filename().string())) {
        errors.record({ErrorKind::WriteFailure, "checkpoint", -1, -1, -1,
                       "unsafe chunk filename rejected"});
        return false;
    }
    // 先写临时文件再原子改名，避免半写状态。
    auto tmp = path;
    tmp += ".tmp";
    {
        std::ofstream out(tmp, std::ios::trunc);
        if (!out) {
            errors.record({ErrorKind::WriteFailure, "checkpoint", -1, -1, -1,
                           "cannot open chunk file for write"});
            return false;
        }
        out << "MBCHK1 " << m.chunk_id << ' ' << m.first_seq << ' '
            << m.last_seq << ' ' << m.packet_count << ' ' << m.payload_bytes
            << ' ' << m.checksum << ' ' << m.input_digest << ' '
            << meta_seal(m) << '\n';
        out.flush();
        if (!out) {
            errors.record({ErrorKind::WriteFailure, "checkpoint", -1, -1, -1,
                           "chunk file write failed"});
            return false;
        }
    }
    std::filesystem::rename(tmp, path, ec);
    if (ec) {
        errors.record({ErrorKind::WriteFailure, "checkpoint", -1, -1, -1,
                       "chunk rename failed: " + ec.message()});
        return false;
    }
    committed_packets_ += m.packet_count;
    committed_bytes_ += m.payload_bytes;
    chunks_.push_back(m);
    return true;
}

uint64_t CheckpointIndex::resume_sequence() const {
    if (chunks_.empty()) return 0;
    return chunks_.back().last_seq + 1;
}

uint64_t CheckpointIndex::committed_packets() const { return committed_packets_; }
uint64_t CheckpointIndex::committed_bytes() const { return committed_bytes_; }

} // namespace media
