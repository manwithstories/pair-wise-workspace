#include "checkpoint.hpp"

#include <fstream>
#include <sstream>
#include <stdexcept>

#include "util.hpp"

namespace mb {

namespace fs = std::filesystem;
constexpr const char* kMagic = "MBCHK1";

std::string CheckpointStore::compute_digest(const std::string& manifest) {
  return sha256_hex(manifest);
}

std::filesystem::path CheckpointStore::block_path(int id) const {
  char name[32];
  std::snprintf(name, sizeof(name), "block_%06d.bin", id);
  return dir_ / name;
}

bool CheckpointStore::open(const CheckpointConfig& cfg, CpError* err) {
  dir_ = cfg.work_root / "checkpoint";
  index_path_ = dir_ / "index.cpi";
  input_digest_ = cfg.input_digest;
  run_id_ = cfg.run_id;
  // 工作目录约束：检查点目录必须位于 work_root 之内。
  if (!path_within(cfg.work_root, dir_)) {
    if (err) *err = {kCpIo, "checkpoint dir escapes work root", dir_.string()};
    return false;
  }
  std::string ioerr;
  if (!ensure_dir(dir_, &ioerr)) {
    if (err) *err = {kCpIo, ioerr, dir_.string()};
    return false;
  }
  if (fs::exists(index_path_)) {
    if (!load_index(err)) return false;  // 索引损坏/摘要变化：拒绝静默复用
  }
  return true;
}

bool CheckpointStore::load_index(CpError* err) {
  std::ifstream f(index_path_, std::ios::binary);
  if (!f) { if (err) *err = {kCpIo, "cannot open index", index_path_.string()};
    return false; }
  std::ostringstream all;
  all << f.rdbuf();
  std::string content = all.str();
  // 切行，定位末行 X 校验和。
  std::vector<std::string> lines;
  size_t pos = 0;
  while (pos < content.size()) {
    size_t nl = content.find('\n', pos);
    if (nl == std::string::npos) nl = content.size();
    lines.push_back(content.substr(pos, nl - pos));
    pos = nl + 1;
  }
  auto fail = [&](const char* code, const std::string& msg, size_t ln) {
    if (err)
      *err = {code, msg, index_path_.string() + ":line " +
                           std::to_string(ln + 1)};
  };
  if (lines.empty() || lines.back().rfind("X:", 0) != 0) {
    fail(kCpIndexCorrupt, "index truncated: missing checksum trailer",
         lines.size());
    return false;
  }
  std::string want = lines.back().substr(2);
  // 校验和覆盖 X 行之前的全部原始字节（含换行）。
  size_t trailer = content.rfind("X:");
  std::string got = sha256_hex(content.data(), trailer);
  if (got != want) { fail(kCpIndexCorrupt, "index checksum mismatch",
                          lines.size() - 1); return false; }
  if (lines.empty() || lines[0] != std::string(kMagic)) {
    fail(kCpIndexCorrupt, "bad magic", 0); return false;
  }
  blocks_.clear();
  finalized_ = false;
  for (size_t i = 1; i + 1 < lines.size(); ++i) {
    const std::string& l = lines[i];
    if (l.rfind("H:", 0) == 0) {
      if (l.substr(2) != input_digest_) {
        fail(kCpInputChanged, "input digest differs from checkpoint", i);
        return false;
      }
    } else if (l.rfind("R:", 0) == 0) {
      if (!run_id_.empty() && l.substr(2) != run_id_) {
        fail(kCpInputChanged, "run id mismatch", i);
        return false;
      }
    } else if (l.rfind("Z:", 0) == 0) {
      finalized_ = (l.substr(2) == "1");
    } else if (l.rfind("B:", 0) == 0 || l.rfind("F:", 0) == 0) {
      // 字段：id,start,end,frames,complete,size,sha（失败行 sha 可能缺失）。
      std::vector<std::string> fld;
      size_t fp = 2;
      while (fp < l.size()) {
        size_t comma = l.find(',', fp);
        fld.push_back(l.substr(fp, comma == std::string::npos
                                          ? std::string::npos
                                          : comma - fp));
        if (comma == std::string::npos) break;
        fp = comma + 1;
      }
      if (fld.size() < 6) {
        fail(kCpIndexCorrupt, "malformed block record", i);
        return false;
      }
      BlockRecord r;
      try {
        r.id = std::stoi(fld[0]);
        r.start_ticks = std::stoll(fld[1]);
        r.end_ticks = std::stoll(fld[2]);
        r.frame_count = std::stoll(fld[3]);
        r.complete = std::stoi(fld[4]) != 0;
        r.size = std::stoull(fld[5]);
      } catch (const std::exception&) {
        fail(kCpIndexCorrupt, "non-numeric block field", i);
        return false;
      }
      r.sha256 = fld.size() >= 7 ? fld[6] : std::string("-");
      blocks_.push_back(r);
    }
  }
  return true;
}

bool CheckpointStore::flush_index(CpError* err) {
  std::ostringstream os;
  os << kMagic << '\n';
  os << "H:" << input_digest_ << '\n';
  os << "R:" << run_id_ << '\n';
  for (const BlockRecord& r : blocks_) {
    if (r.complete)
      os << "B:" << r.id << ',' << r.start_ticks << ',' << r.end_ticks << ','
         << r.frame_count << ",1," << r.size << ',' << r.sha256 << '\n';
    else
      os << "F:" << r.id << ',' << r.start_ticks << ',' << r.end_ticks
         << ',' << r.frame_count << ",0,0,-\n";  // 与 B 行同为 7 字段
  }
  if (finalized_) os << "Z:1\n";
  std::string body = os.str();
  std::string sum = sha256_hex(body);
  body += "X:" + sum + "\n";
  auto tmp = index_path_; tmp += ".tmp";
  std::ofstream f(tmp, std::ios::binary | std::ios::trunc);
  if (!f) { if (err) *err = {kCpIo, "cannot write index tmp", tmp.string()};
    return false; }
  f.write(body.data(), static_cast<std::streamsize>(body.size()));
  f.close();
  std::error_code ec;
  fs::rename(tmp, index_path_, ec);
  if (ec) {
    if (err)
      *err = {kCpIo, "index rename failed: " + ec.message(),
              index_path_.string()};
    return false;
  }
  return true;
}

bool CheckpointStore::write_block(int id, int64_t start, int64_t end,
                                  int64_t frames,
                                  const std::vector<uint8_t>& payload,
                                  CpError* err) {
  auto path = block_path(id);
  auto tmp = path; tmp += ".tmp";
  std::ofstream f(tmp, std::ios::binary | std::ios::trunc);
  if (!f) { if (err) *err = {kCpIo, "cannot open block tmp", tmp.string()};
    return false; }
  f.write(reinterpret_cast<const char*>(payload.data()),
          static_cast<std::streamsize>(payload.size()));
  f.close();
  std::error_code ec;
  fs::rename(tmp, path, ec);
  if (ec) {
    if (err) *err = {kCpIo, "block rename failed", path.string()};
    return false;
  }
  BlockRecord r;
  r.id = id; r.start_ticks = start; r.end_ticks = end; r.frame_count = frames;
  r.complete = true; r.size = payload.size();
  r.sha256 = sha256_hex(payload.data(), payload.size());
  for (auto& b : blocks_) if (b.id == id) { b = r; return flush_index(err); }
  blocks_.push_back(r);
  return flush_index(err);
}

bool CheckpointStore::mark_block_failed(int id, int64_t start, int64_t end,
                                        CpError* err) {
  for (auto& b : blocks_)
    if (b.id == id) { b = {id, start, end, 0, false, "-", 0};
      return flush_index(err); }
  blocks_.push_back({id, start, end, 0, false, "-", 0});
  return flush_index(err);
}

bool CheckpointStore::resume(ResumeResult* out, CpError* err) {
  if (fs::exists(index_path_) && !load_index(err)) return false;
  ResumeResult rr;
  for (const BlockRecord& b : blocks_) {
    if (!b.complete) { rr.failed_blocks.push_back(b.id); continue; }
    auto p = block_path(b.id);
    std::string herr;
    if (!fs::exists(p) || mb::file_size(p) != b.size) {
      if (err) *err = {kCpBlockTruncated, "block missing or size mismatch",
                       p.string()};
      return false;
    }
    std::string h = sha256_file(p, &herr);
    if (h.empty() || h != b.sha256) {
      if (err) *err = {kCpBlockHash, "block hash mismatch", p.string()};
      return false;
    }
    rr.complete_blocks.push_back(b.id);
  }
  rr.resumed_blocks = rr.complete_blocks.size();
  if (out) *out = rr;
  return true;
}

bool CheckpointStore::finalize(CpError* err) {
  finalized_ = true;
  return flush_index(err);
}

}  // namespace mb
