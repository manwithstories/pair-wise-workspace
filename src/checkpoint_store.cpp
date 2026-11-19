#include "media/checkpoint_index.hpp"
#include "media/checksum.hpp"
#include "media/checkpoint_path.hpp"

#include <algorithm>
#include <filesystem>
#include <fstream>
#include <sstream>
#include <system_error>

namespace media {

namespace {
bool safe_path(const std::string& p) { return checkpoint_path_safe(p); }
} // namespace

// Compute the canonical CRC for a block from its identity fields.
std::uint32_t block_crc(const CheckpointBlock& b) {
  std::ostringstream os;
  os << b.block_id << '|' << b.input_hash << '|' << b.start_us << '|'
     << b.end_us << '|' << b.packet_count;
  const std::string payload = os.str();
  return crc32(reinterpret_cast<const std::uint8_t*>(payload.data()),
               payload.size());
}

// Rewrite the manifest from the set of chunk files actually present, so a
// crash between chunk publish and manifest update cannot lose a block.
static bool refresh_manifest(const CheckpointIndex& cp,
                             const CheckpointState& state) {
  std::ostringstream os;
  os << "media_manifest_v1\n";
  os << "run_input_hash=" << state.run_input_hash << "\n";
  os << "codec=" << state.codec << "\n";
  os << "total_blocks=" << state.total_blocks << "\n";
  for (const auto& b : state.blocks)
    os << "block " << b.block_id << " " << b.crc32 << "\n";
  std::string error;
  // Reuse the atomic writer through a small public-free helper path.
  const std::string tmp = cp.dir() + "/manifest.txt.tmp";
  {
    std::ofstream out(tmp, std::ios::binary | std::ios::trunc);
    if (!out) return false;
    out << os.str();
    out.flush();
    if (!out.good()) return false;
  }
  std::error_code ec;
  std::filesystem::rename(tmp, cp.dir() + "/manifest.txt", ec);
  return !ec;
}

bool CheckpointIndex::write_block(const CheckpointBlock& in,
                                  std::string& error) {
  if (!safe_path(dir_)) { error = "unsafe checkpoint path"; return false; }
  CheckpointBlock block = in;
  block.crc32 = block_crc(block);

  // Hidden temp then rename: partial files are never valid checkpoints.
  const std::string final_path = chunk_path(block.block_id);
  const std::string tmp = final_path + ".partial";
  {
    std::ofstream out(tmp, std::ios::binary | std::ios::trunc);
    if (!out) { error = "cannot open temp chunk"; return false; }
    out << serialize_block(block);
    out.flush();
    if (!out.good()) { error = "chunk write failed"; return false; }
  }
  std::error_code ec;
  std::filesystem::rename(tmp, final_path, ec);
  if (ec) { error = "atomic rename failed"; return false; }

  CheckpointStatus st;
  CheckpointState state = load(st);
  state.run_input_hash = in.input_hash ? in.input_hash : state.run_input_hash;
  state.total_blocks = std::max(state.total_blocks, block.block_id + 1);
  bool present = false;
  for (auto& b : state.blocks)
    if (b.block_id == block.block_id) { b = block; present = true; }
  if (!present) state.blocks.push_back(block);
  std::sort(state.blocks.begin(), state.blocks.end(),
            [](const CheckpointBlock& a, const CheckpointBlock& b) {
              return a.block_id < b.block_id;
            });
  return refresh_manifest(*this, state);
}

bool CheckpointIndex::simulate_interrupt(const CheckpointBlock& block,
                                         std::string& error) {
  if (!safe_path(dir_)) { error = "unsafe checkpoint path"; return false; }
  // Write only the hidden partial file and never publish it.
  const std::string tmp = chunk_path(block.block_id) + ".partial";
  std::ofstream out(tmp, std::ios::binary | std::ios::trunc);
  if (!out) { error = "cannot open interrupt temp"; return false; }
  CheckpointBlock with_crc = block;
  with_crc.crc32 = block_crc(block);
  out << serialize_block(with_crc);
  return out.good();
}

CheckpointState CheckpointIndex::load(CheckpointStatus& status) const {
  CheckpointState state;
  status = CheckpointStatus::Ok;
  if (!safe_path(dir_)) { status = CheckpointStatus::Unrecoverable; return state; }
  std::error_code ec;
  if (!std::filesystem::exists(dir_, ec) ||
      !std::filesystem::exists(manifest_path(), ec))
    return state; // fresh run: nothing to resume yet

  std::ifstream mf(manifest_path());
  if (!mf) { status = CheckpointStatus::Unrecoverable; return state; }
  std::string line;
  if (!std::getline(mf, line) || line != "media_manifest_v1") {
    status = CheckpointStatus::Unrecoverable;
    return state;
  }
  while (std::getline(mf, line)) {
    if (line.rfind("run_input_hash=", 0) == 0)
      state.run_input_hash = std::stoull(line.substr(15));
    else if (line.rfind("codec=", 0) == 0)
      state.codec = static_cast<std::uint32_t>(std::stoul(line.substr(6)));
    else if (line.rfind("total_blocks=", 0) == 0)
      state.total_blocks = static_cast<std::uint32_t>(
          std::stoul(line.substr(13)));
  }

  // Verify each referenced chunk from disk; skip corrupt ones and roll back.
  std::uint32_t expected_next = 0;
  for (std::uint32_t id = 0; id < state.total_blocks; ++id) {
    const std::string path = chunk_path(id);
    if (!std::filesystem::exists(path, ec)) continue;
    std::ifstream cf(path);
    std::stringstream ss;
    ss << cf.rdbuf();
    CheckpointBlock block;
    std::string err;
    if (!parse_block(ss.str(), block, err) ||
        !verify_block(block, state.run_input_hash, err)) {
      status = CheckpointStatus::RolledBack;
      continue; // corrupt chunk: not part of the valid prefix
    }
    if (block.block_id != expected_next) {
      status = CheckpointStatus::RolledBack;
      continue; // gap: resume must rebuild from here
    }
    state.blocks.push_back(block);
    ++expected_next;
  }
  if (!state.blocks.empty())
    status = status == CheckpointStatus::RolledBack
        ? CheckpointStatus::RolledBack : CheckpointStatus::Resumed;
  return state;
}

} // namespace media
