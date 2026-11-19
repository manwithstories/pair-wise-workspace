#include <algorithm>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>

#include "media_archive/checkpoint_store.h"

namespace media_archive {
namespace {
constexpr char kBlockMagic[8] = {'M', 'A', 'C', 'P', '1', 0, 0, 0};
constexpr char kManifestMagic[8] = {'M', 'A', 'C', 'M', '1', 0, 0, 0};

void put_u32(std::ofstream& f, uint32_t v) {
  char b[4];
  for (int i = 0; i < 4; ++i) b[i] = static_cast<char>((v >> (8 * i)) & 0xFF);
  f.write(b, 4);
}
void put_u64(std::ofstream& f, uint64_t v) {
  char b[8];
  for (int i = 0; i < 8; ++i) b[i] = static_cast<char>((v >> (8 * i)) & 0xFF);
  f.write(b, 8);
}
bool get_u32(std::ifstream& f, uint32_t* v) {
  unsigned char b[4];
  if (!f.read(reinterpret_cast<char*>(b), 4)) return false;
  *v = 0;
  for (int i = 0; i < 4; ++i) *v |= uint32_t(b[i]) << (8 * i);
  return true;
}
bool get_u64(std::ifstream& f, uint64_t* v) {
  unsigned char b[8];
  if (!f.read(reinterpret_cast<char*>(b), 8)) return false;
  *v = 0;
  for (int i = 0; i < 8; ++i) *v |= uint64_t(b[i]) << (8 * i);
  return true;
}

// Read and validate one block file. Returns false on any corruption.
bool read_block(const std::string& path, uint64_t expected_index,
                std::vector<KeyframeEntry>* out) {
  std::ifstream f(path, std::ios::binary);
  if (!f) return false;
  char magic[8];
  if (!f.read(magic, 8) || std::memcmp(magic, kBlockMagic, 8) != 0)
    return false;
  uint32_t block_index = 0, count = 0, stored_crc = 0;
  if (!get_u32(f, &block_index) || !get_u32(f, &count) ||
      !get_u32(f, &stored_crc))
    return false;
  if (block_index != expected_index ||
      count > kMaxKeyframesPerBlock)
    return false;
  std::vector<KeyframeEntry> entries;
  entries.reserve(count);
  uint32_t running = 0;
  for (uint32_t i = 0; i < count; ++i) {
    KeyframeEntry e;
    if (!get_u64(f, &e.pts_ticks) || !get_u64(f, &e.source_offset) ||
        !get_u32(f, &e.size_bytes) || !get_u32(f, &e.frame_type) ||
        !get_u32(f, &e.extended_seq) || !get_u32(f, &e.checksum))
      return false;
    running = crc32_ieee(reinterpret_cast<const uint8_t*>(&e), sizeof(e)) ^
              running;
    entries.push_back(e);
  }
  if (running != stored_crc) return false;
  *out = std::move(entries);
  return true;
}
}  // namespace

std::string CheckpointStore::block_path(uint64_t block) const {
  char name[32];
  std::snprintf(name, sizeof(name), "/block-%06u.cp",
                static_cast<unsigned>(block));
  return dir_ + name;
}
std::string CheckpointStore::temp_path(uint64_t block) const {
  return block_path(block) + ".tmp";
}
std::string CheckpointStore::manifest_path() const {
  return dir_ + "/manifest.mac";
}

CheckpointStore::~CheckpointStore() {
  if (open_) {
    // Best-effort flush; destructors must not throw.
    (void)flush();
  }
}

Diagnostic CheckpointStore::write_block(
    uint64_t block, const std::vector<KeyframeEntry>& entries) {
  if (entries.size() > cfg_.max_records_per_block)
    return diag(Status::UsageError, "checkpoint block exceeds record limit");
  std::error_code ec;
  std::filesystem::create_directories(dir_, ec);
  if (ec) return diag(Status::IoError,
                      "cannot create checkpoint dir: " + dir_);
  const std::string tmp = temp_path(block);
  {
    std::ofstream f(tmp, std::ios::binary | std::ios::trunc);
    if (!f) return diag(Status::IoError, "cannot open block: " + tmp);
    f.write(kBlockMagic, 8);
    put_u32(f, static_cast<uint32_t>(block));
    put_u32(f, static_cast<uint32_t>(entries.size()));
    uint32_t running = 0;
    for (const auto& e : entries)
      running =
          crc32_ieee(reinterpret_cast<const uint8_t*>(&e), sizeof(e)) ^
          running;
    put_u32(f, running);
    for (const auto& e : entries) {
      put_u64(f, e.pts_ticks);
      put_u64(f, e.source_offset);
      put_u32(f, e.size_bytes);
      put_u32(f, e.frame_type);
      put_u32(f, e.extended_seq);
      put_u32(f, e.checksum);
    }
    if (!f) return diag(Status::IoError, "block write failed: " + tmp);
  }
  // Atomic publish: rename only after the temp block is fully durable.
  std::filesystem::rename(tmp, block_path(block), ec);
  if (ec) return diag(Status::IoError, "block rename failed: " + block_path(block));
  return {};
}

Diagnostic CheckpointStore::open(const std::string& dir,
                                 const CheckpointConfig& config) {
  cfg_ = config;
  dir_ = dir;
  std::error_code ec;
  std::filesystem::create_directories(dir_, ec);
  if (ec) return diag(Status::IoError,
                      "cannot create checkpoint dir: " + dir_);
  CheckpointOpenResult recovered;
  Diagnostic d = open_checkpoint_dir(dir_, cfg_, &recovered);
  if (!d.ok()) return d;
  // Recovered records already live in durable block files; the pending
  // buffer only holds brand-new appends, otherwise a resume would rewrite
  // historical entries and duplicate them.
  pending_.clear();
  next_block_ = recovered.last_valid_block == UINT64_MAX
                    ? 0
                    : recovered.last_valid_block + 1;
  durable_records_ = recovered.recovered.size();
  rolled_back_block_ = recovered.rolled_back_block;
  total_appends_ = recovered.recovered.size();
  open_ = true;
  return {};
}

Diagnostic CheckpointStore::append(const KeyframeEntry& entry,
                                   uint64_t crash_after_appends) {
  if (!open_) return diag(Status::IoError, "checkpoint store not open");
  ++total_appends_;
  if (total_appends_ > crash_after_appends) {
    // Simulated crash: discard the in-flight block and refuse every later
    // flush so uncommitted appends can never be resurrected.
    crashed_ = true;
    return diag(Status::IoError,
                "simulated crash before block finalization");
  }
  pending_.push_back(entry);
  if (pending_.size() >= cfg_.records_per_block) {
    Diagnostic d = write_block(next_block_, pending_);
    if (!d.ok()) return d;
    ++next_block_;
    pending_.clear();
  }
  return {};
}

Diagnostic CheckpointStore::flush() {
  if (!open_ || crashed_) return {};
  if (!pending_.empty()) {
    Diagnostic d = write_block(next_block_, pending_);
    if (!d.ok()) return d;
    ++next_block_;
    pending_.clear();
  }
  // Manifest points at the last durable block.
  const std::string mp = manifest_path();
  const std::string mtmp = mp + ".tmp";
  {
    std::ofstream f(mtmp, std::ios::binary | std::ios::trunc);
    if (!f) return diag(Status::IoError, "cannot write manifest: " + mtmp);
    f.write(kManifestMagic, 8);
    put_u64(f, next_block_ == 0 ? 0 : next_block_ - 1);
    put_u64(f, total_appends_);
    if (!f) return diag(Status::IoError, "manifest write failed: " + mtmp);
  }
  std::error_code ec;
  std::filesystem::rename(mtmp, mp, ec);
  if (ec) return diag(Status::IoError, "manifest rename failed: " + mp);
  return {};
}

Diagnostic open_checkpoint_dir(const std::string& dir,
                               const CheckpointConfig& config,
                               CheckpointOpenResult* result) {
  (void)config;
  std::error_code ec;
  if (!std::filesystem::exists(dir, ec)) {
    result->resumed = false;
    return {};
  }
  std::vector<std::filesystem::path> blocks;
  for (const auto& de : std::filesystem::directory_iterator(dir, ec)) {
    const std::string name = de.path().filename().string();
    if (name.rfind("block-", 0) == 0 &&
        name.size() > 3 && name.substr(name.size() - 3) == ".cp")
      blocks.push_back(de.path());
  }
  if (ec) return diag(Status::IoError, "cannot scan checkpoint dir: " + dir);
  std::sort(blocks.begin(), blocks.end());

  result->recovered.clear();
  result->last_valid_block = UINT64_MAX;
  result->rolled_back_block = UINT64_MAX;
  result->resumed = !blocks.empty();

  for (uint64_t index = 0; index < blocks.size(); ++index) {
    std::vector<KeyframeEntry> entries;
    if (!read_block(blocks[index].string(), index, &entries)) {
      // Corrupt tail (or a gap): roll back to the previous valid block.
      result->rolled_back_block = index;
      break;
    }
    result->recovered.insert(result->recovered.end(),
                             entries.begin(), entries.end());
    result->last_valid_block = index;
  }
  return {};
}

}  // namespace media_archive
