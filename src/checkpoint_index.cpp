#include "media/checkpoint_index.hpp"
#include "media/checksum.hpp"
#include "media/checkpoint_path.hpp"

#include <cstdio>
#include <filesystem>
#include <fstream>
#include <sstream>
#include <system_error>

namespace media {

bool checkpoint_path_safe(const std::string& p) {
  if (p.empty() || p.find('\0') != std::string::npos) return false;
  if (p.find("..") != std::string::npos) return false;
  return true;
}

namespace {

// Local convenience alias used within this translation unit.
bool safe_path(const std::string& p) { return checkpoint_path_safe(p); }

} // namespace

CheckpointIndex::CheckpointIndex(std::string dir) : dir_(std::move(dir)) {
  if (safe_path(dir_)) {
    std::error_code ec;
    std::filesystem::create_directories(dir_, ec);
  }
}

std::string CheckpointIndex::manifest_path() const {
  return dir_ + "/manifest.txt";
}

std::string CheckpointIndex::chunk_path(std::uint32_t id) const {
  char name[32];
  std::snprintf(name, sizeof(name), "/chunk_%05u.txt", id);
  return dir_ + name;
}

std::uint32_t CheckpointIndex::completed_count() const noexcept {
  CheckpointStatus st;
  return static_cast<std::uint32_t>(load(st).blocks.size());
}

std::string serialize_block(const CheckpointBlock& b) {
  std::ostringstream os;
  os << "media_checkpoint_v1\n";
  os << "block_id=" << b.block_id << "\n";
  os << "input_hash=" << b.input_hash << "\n";
  os << "start_us=" << b.start_us << "\n";
  os << "end_us=" << b.end_us << "\n";
  os << "packet_count=" << b.packet_count << "\n";
  os << "crc32=" << b.crc32 << "\n";
  return os.str();
}

bool parse_block(const std::string& text, CheckpointBlock& out,
                 std::string& error) {
  std::istringstream is(text);
  std::string line;
  if (!std::getline(is, line) || line != "media_checkpoint_v1") {
    error = "bad checkpoint header";
    return false;
  }
  out = CheckpointBlock{};
  bool have_crc = false;
  while (std::getline(is, line)) {
    std::istringstream ls(line);
    std::string key;
    if (!std::getline(ls, key, '=')) continue;
    std::string val;
    std::getline(ls, val);
    try {
      if (key == "block_id") out.block_id = static_cast<std::uint32_t>(std::stoul(val));
      else if (key == "input_hash") out.input_hash = std::stoull(val);
      else if (key == "start_us") out.start_us = std::stoll(val);
      else if (key == "end_us") out.end_us = std::stoll(val);
      else if (key == "packet_count") out.packet_count = std::stoull(val);
      else if (key == "crc32") { out.crc32 = static_cast<std::uint32_t>(std::stoul(val)); have_crc = true; }
      else { error = "unknown key " + key; return false; }
    } catch (...) {
      error = "bad value for " + key;
      return false;
    }
  }
  if (!have_crc) { error = "missing crc32"; return false; }
  return true;
}

bool verify_block(const CheckpointBlock& b, std::uint64_t run_input_hash,
                  std::string& error) {
  if (b.input_hash != run_input_hash) {
    error = "input digest mismatch on block " + std::to_string(b.block_id);
    return false;
  }
  // Recompute the CRC over the canonical payload description.
  std::ostringstream os;
  os << b.block_id << '|' << b.input_hash << '|' << b.start_us << '|'
     << b.end_us << '|' << b.packet_count;
  const std::string payload = os.str();
  const std::uint32_t expect =
      crc32(reinterpret_cast<const std::uint8_t*>(payload.data()),
            payload.size());
  if (expect != b.crc32) {
    error = "crc mismatch on block " + std::to_string(b.block_id);
    return false;
  }
  return true;
}

} // namespace media
