#include "relay/segment_writer.hpp"

#include <cstdio>
#include <cstring>
#include <filesystem>

extern "C" {
#include <libavutil/mem.h>
#include <libavutil/sha.h>
}

namespace fs = std::filesystem;

namespace relay {

namespace {
Sha256 file_sha256(const std::string& path, uint64_t& bytes_out) {
  Sha256 out{};
  bytes_out = 0;
  std::FILE* f = std::fopen(path.c_str(), "rb");
  if (!f) return out;
  AVSHA* ctx = av_sha_alloc();
  av_sha_init(ctx, 256);
  std::vector<uint8_t> buf(1 << 16);
  size_t n;
  while ((n = std::fread(buf.data(), 1, buf.size(), f)) > 0) {
    av_sha_update(ctx, buf.data(), n);
    bytes_out += n;
  }
  av_sha_final(ctx, out.data());
  av_free(ctx);
  std::fclose(f);
  return out;
}

std::string segment_name(uint32_t id, const char* ext) {
  char buf[64];
  std::snprintf(buf, sizeof(buf), "segment_%05u.%s", id, ext);
  return buf;
}
} // namespace

SegmentWriter::SegmentWriter(std::string out_dir, uint32_t frames_per_segment)
    : out_dir_(std::move(out_dir)), frames_per_segment_(frames_per_segment) {
  std::error_code ec;
  fs::create_directories(out_dir_, ec);
  if (ec) failed_ = true;
}

SegmentWriter::~SegmentWriter() {
  if (cur_) std::fclose(cur_); // drop unsealed temp segment
}

std::string SegmentWriter::tmp_path(uint32_t id) const {
  return out_dir_ + "/" + segment_name(id, "bin.tmp");
}
std::string SegmentWriter::bin_path(uint32_t id) const {
  return out_dir_ + "/" + segment_name(id, "bin");
}
std::string SegmentWriter::done_path(uint32_t id) const {
  return out_dir_ + "/" + segment_name(id, "done");
}

bool SegmentWriter::verify_file(const std::string& path,
                                const Sha256& expected,
                                uint64_t expected_bytes) {
  uint64_t bytes = 0;
  Sha256 got = file_sha256(path, bytes);
  return bytes == expected_bytes && got == expected;
}

bool SegmentWriter::recover() {
  std::error_code ec;
  // Discard incomplete temp files: they were never sealed.
  for (const auto& e : fs::directory_iterator(out_dir_, ec)) {
    if (e.path().extension() == ".tmp") fs::remove(e.path(), ec);
  }
  // Re-admit only manifest segments whose hash verifies.
  completed_.clear();
  std::FILE* m = std::fopen((out_dir_ + "/segments.manifest").c_str(), "r");
  if (m) {
    char line[256];
    while (std::fgets(line, sizeof(line), m)) {
      char name[64] = {0}, hash[65] = {0};
      unsigned long long frames = 0, bytes = 0;
      if (std::sscanf(line, "%63s %64s %llu %llu", name, hash, &frames,
                      &bytes) != 4)
        continue;
      SegmentInfo info;
      if (std::sscanf(name, "segment_%u.bin", &info.id) != 1) continue;
      info.path = out_dir_ + "/" + name;
      info.frames = frames;
      info.bytes = bytes;
      for (int i = 0; i < 32; ++i)
        info.sha256[i] = static_cast<uint8_t>(
            std::stoul(std::string(hash + i * 2, 2), nullptr, 16));
      if (verify_file(info.path, info.sha256, info.bytes))
        completed_.push_back(info); // only verified segments resume
    }
    std::fclose(m);
  }
  current_id_ = completed_.empty() ? 0 : completed_.back().id + 1;
  recovered_count_ = completed_.size();
  return !failed_;
}

uint64_t SegmentWriter::write_frame(const FrameRecord& rec) {
  if (failed_) return UINT64_MAX;
  if (!cur_) {
    cur_ = std::fopen(tmp_path(current_id_).c_str(), "wb");
    if (!cur_) {
      failed_ = true;
      return UINT64_MAX;
    }
    cur_frames_ = 0;
    cur_bytes_ = 0;
  }
  // Serialize with the same on-wire record layout as the container.
  std::vector<uint8_t> b;
  auto put32 = [&](uint32_t v) {
    for (int i = 0; i < 4; ++i) b.push_back(static_cast<uint8_t>(v >> (i * 8)));
  };
  auto put64 = [&](uint64_t v) {
    for (int i = 0; i < 8; ++i) b.push_back(static_cast<uint8_t>(v >> (i * 8)));
  };
  constexpr uint32_t kFixed = 4 + 8 + 8 + 4 + 4 + 1 + 3 + 4 + 32;
  put32(kFixed + static_cast<uint32_t>(rec.payload.size()));
  put64(rec.frame_seq);
  put64(static_cast<uint64_t>(rec.node_clock));
  put32(static_cast<uint32_t>(rec.time_base.num));
  put32(static_cast<uint32_t>(rec.time_base.den));
  b.push_back(rec.keyframe ? 1 : 0);
  b.insert(b.end(), {0, 0, 0});
  put32(static_cast<uint32_t>(rec.payload.size()));
  b.insert(b.end(), rec.payload.begin(), rec.payload.end());
  b.insert(b.end(), rec.checksum.begin(), rec.checksum.end());

  const uint64_t offset = cur_bytes_;
  if (std::fwrite(b.data(), 1, b.size(), cur_) != b.size()) {
    failed_ = true;
    return UINT64_MAX;
  }
  cur_bytes_ += b.size();
  ++cur_frames_;
  if (cur_frames_ >= frames_per_segment_ && !seal_current()) return UINT64_MAX;
  return offset;
}

bool SegmentWriter::seal_current() {
  if (!cur_) return true;
  if (std::fclose(cur_) != 0) {
    cur_ = nullptr;
    failed_ = true;
    return false;
  }
  cur_ = nullptr;
  SegmentInfo info;
  info.id = current_id_;
  info.path = bin_path(current_id_);
  info.frames = cur_frames_;
  info.sha256 = file_sha256(tmp_path(current_id_), info.bytes);
  std::error_code ec;
  fs::rename(tmp_path(current_id_), info.path, ec);
  if (ec) {
    failed_ = true;
    return false;
  }
  // Done marker: the segment is only complete once this file exists.
  std::FILE* d = std::fopen(done_path(current_id_).c_str(), "w");
  if (!d || std::fprintf(d, "%s %llu\n", hex(info.sha256).c_str(),
                         static_cast<unsigned long long>(info.bytes)) < 0 ||
      std::fclose(d) != 0) {
    failed_ = true;
    return false;
  }
  completed_.push_back(info);
  ++current_id_;
  return flush_manifest();
}

bool SegmentWriter::flush_manifest() const {
  const std::string path = out_dir_ + "/segments.manifest";
  std::FILE* m = std::fopen(path.c_str(), "w");
  if (!m) return false;
  for (const auto& s : completed_)
    std::fprintf(m, "%s %s %llu %llu\n",
                 segment_name(s.id, "bin").c_str(), hex(s.sha256).c_str(),
                 static_cast<unsigned long long>(s.frames),
                 static_cast<unsigned long long>(s.bytes));
  return std::fclose(m) == 0;
}

bool SegmentWriter::finalize() {
  if (failed_) return false;
  if (!seal_current()) return false;
  return flush_manifest();
}

} // namespace relay
