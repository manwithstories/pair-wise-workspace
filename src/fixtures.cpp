#include "fixtures.hpp"

#include <fcntl.h>
#include <sys/wait.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <vector>

#include "config.hpp"
#include "mp4_structure.hpp"
#include "resume_store.hpp"
#include "sha256.hpp"
#include "util.hpp"

namespace mediaqc {
namespace {

bool read_all(const std::filesystem::path& path, std::string& out, std::string& err) {
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    err = "cannot open " + path.string();
    return false;
  }
  in.seekg(0, std::ios::end);
  const auto len = in.tellg();
  if (len < 0) {
    err = "cannot size " + path.string();
    return false;
  }
  in.seekg(0, std::ios::beg);
  out.resize(static_cast<size_t>(len));
  if (len > 0 && !in.read(out.data(), len)) {
    err = "short read on " + path.string();
    return false;
  }
  return true;
}

bool write_all(const std::filesystem::path& path, const std::string& data,
               std::string& err) {
  std::ofstream out(path, std::ios::binary | std::ios::trunc);
  if (!out) {
    err = "cannot write " + path.string();
    return false;
  }
  out.write(data.data(), static_cast<std::streamsize>(data.size()));
  if (!out) {
    err = "short write on " + path.string();
    return false;
  }
  return true;
}

void put_be32(std::string& s, size_t at, uint32_t v) {
  if (at + 4 > s.size()) return;
  s[at] = char(v >> 24); s[at + 1] = char(v >> 16);
  s[at + 2] = char(v >> 8); s[at + 3] = char(v);
}
void put_be64(std::string& s, size_t at, uint64_t v) {
  put_be32(s, at, uint32_t(v >> 32));
  put_be32(s, at + 4, uint32_t(v & 0xFFFFFFFFu));
}

bool find_nested(const std::string& data, const std::string& path, size_t& payload_at,
                 size_t& payload_len, size_t& header_at) {
  // path like "moov/trak/mdia/minf/stbl/stco"
  std::vector<std::string> parts;
  std::string cur;
  for (char c : path) {
    if (c == '/') {
      parts.push_back(cur);
      cur.clear();
    } else {
      cur += c;
    }
  }
  parts.push_back(cur);
  size_t start = 0;
  size_t end = data.size();
  size_t hdr = 0;
  for (size_t i = 0; i < parts.size(); ++i) {
    const std::string& want = parts[i];
    bool found = false;
    size_t off = start;
    while (off + 8 <= end) {
      const uint32_t size32 = (uint32_t(uint8_t(data[off])) << 24) |
                              (uint32_t(uint8_t(data[off + 1])) << 16) |
                              (uint32_t(uint8_t(data[off + 2])) << 8) |
                              uint8_t(data[off + 3]);
      const std::string t = data.substr(off + 4, 4);
      size_t header = 8;
      uint64_t size = size32;
      if (size32 == 1 && off + 16 <= end) {
        size = 0;
        for (int k = 0; k < 8; ++k) size = (size << 8) | uint8_t(data[off + 8 + k]);
        header = 16;
      } else if (size32 == 0) {
        size = end - off;
      }
      if (size < header) return false;
      if (t == want) {
        hdr = off;
        start = off + header;
        end = std::min<size_t>(off + static_cast<size_t>(size), data.size());
        found = true;
        break;
      }
      off += static_cast<size_t>(size);
    }
    if (!found) return false;
    if (i + 1 == parts.size()) {
      header_at = hdr;
      payload_at = start;
      payload_len = end - start;
      return true;
    }
  }
  return false;
}

// Runs a program with an argument vector; no shell is involved, so paths with
// spaces or metacharacters are safe.
bool run_process(const std::string& program, const std::vector<std::string>& args,
                 std::string& err) {
  std::vector<char*> argv;
  argv.push_back(const_cast<char*>(program.c_str()));
  for (const auto& a : args) argv.push_back(const_cast<char*>(a.c_str()));
  argv.push_back(nullptr);
  pid_t pid = ::fork();
  if (pid < 0) {
    err = "fork failed";
    return false;
  }
  if (pid == 0) {
    // Silence FFmpeg's banner; stderr stays visible for real failures.
    const int devnull = ::open("/dev/null", O_WRONLY);
    if (devnull >= 0) {
      ::dup2(devnull, 1);
      ::close(devnull);
    }
    ::execvp(program.c_str(), argv.data());
    ::_exit(127);
  }
  int status = 0;
  while (::waitpid(pid, &status, 0) < 0) {
    if (errno != EINTR) {
      err = "waitpid failed";
      return false;
    }
  }
  if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
    err = program + " exited with status " + std::to_string(WEXITSTATUS(status));
    return false;
  }
  return true;
}

FixtureInfo make_info(const std::filesystem::path& path, FixtureKind kind,
                      bool expect_ok, const std::string& description) {
  FixtureInfo info;
  info.name = path.filename().string();
  info.path = path.string();
  info.kind = kind;
  info.expect_ok = expect_ok;
  info.description = description;
  info.input_size = file_size_bytes(path);
  return info;
}

}  // namespace

const char* fixture_kind_name(FixtureKind k) {
  switch (k) {
    case FixtureKind::Clean: return "clean";
    case FixtureKind::MoovAtEnd: return "moov_at_end";
    case FixtureKind::TruncatedTail: return "truncated_tail";
    case FixtureKind::TruncatedMoov: return "truncated_moov";
    case FixtureKind::NoMoov: return "no_moov";
    case FixtureKind::CorruptPacket: return "corrupt_packet";
    case FixtureKind::IndexOutOfRange: return "index_out_of_range";
    case FixtureKind::IncompleteIndex: return "incomplete_index";
    case FixtureKind::TimestampWrap: return "timestamp_wrap";
    case FixtureKind::BackwardsTimestamp: return "backwards_timestamp";
    case FixtureKind::ZeroLength: return "zero_length";
    case FixtureKind::StaleCheckpoint: return "stale_checkpoint";
  }
  return "unknown";
}

bool encode_source(const std::string& ffmpeg_binary, const std::string& path,
                   int seconds, bool faststart, std::string& err) {
  const std::string dur = std::to_string(seconds);
  std::vector<std::string> args = {
      "-hide_banner", "-loglevel", "error", "-y",
      "-f", "lavfi", "-i", "testsrc2=size=128x96:rate=10:duration=" + dur,
      "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=" + dur,
      "-c:v", "mpeg4", "-g", "10", "-q:v", "5",
      "-c:a", "aac", "-b:a", "64k",
      "-movflags", faststart ? "+faststart" : "+faststart+empty_moov",
      path,
  };
  // For the fragmented variant we need real fragments, not just an empty
  // moov; the caller selects that through the file name convention below.
  return run_process(ffmpeg_binary, args, err);
}

bool truncate_file(const std::filesystem::path& path, int64_t keep_bytes, std::string& err) {
  const int64_t size = file_size_bytes(path);
  if (size < 0) {
    err = "cannot size " + path.string();
    return false;
  }
  if (keep_bytes < 0 || keep_bytes > size) {
    err = "truncate target outside file";
    return false;
  }
  if (::truncate(path.c_str(), static_cast<off_t>(keep_bytes)) != 0) {
    err = std::string("truncate failed: ") + std::strerror(errno);
    return false;
  }
  return true;
}

bool drop_moov(const std::filesystem::path& src, const std::filesystem::path& dst,
               std::string& err) {
  std::string data;
  if (!read_all(src, data, err)) return false;
  FileStructure fs;
  Error e;
  if (!read_mp4_structure(src.string(), fs, e) || !fs.has_moov) {
    err = "source has no moov to drop";
    return false;
  }
  const std::string out = data.substr(0, fs.moov_offset) +
                          data.substr(fs.moov_offset + fs.moov_size);
  return write_all(dst, out, err);
}

bool patch_stco_entry(const std::filesystem::path& path, int64_t index,
                      int64_t new_target, std::string& err) {
  std::string data;
  if (!read_all(path, data, err)) return false;
  size_t payload = 0, len = 0, hdr = 0;
  if (!find_nested(data, "moov/trak/mdia/minf/stbl/stco", payload, len, hdr)) {
    err = "no video stco";
    return false;
  }
  const uint32_t count = (uint32_t(uint8_t(data[payload + 4])) << 24) |
                         (uint32_t(uint8_t(data[payload + 5])) << 16) |
                         (uint32_t(uint8_t(data[payload + 6])) << 8) |
                         uint8_t(data[payload + 7]);
  if (index < 0 || index >= int64_t(count) || len < size_t(8 + (index + 1) * 4)) {
    err = "stco entry index out of range";
    return false;
  }
  // full box (4) + count (4) + entries
  put_be32(data, payload + 8 + size_t(index) * 4, static_cast<uint32_t>(new_target));
  return write_all(path, data, err);
}

bool patch_stco_entry_count(const std::filesystem::path& path, int64_t new_count,
                            std::string& err) {
  std::string data;
  if (!read_all(path, data, err)) return false;
  size_t payload = 0, len = 0, hdr = 0;
  if (!find_nested(data, "moov/trak/mdia/minf/stbl/stco", payload, len, hdr)) {
    err = "no video stco";
    return false;
  }
  if (len < 8) {
    err = "stco too short";
    return false;
  }
  // Claim more entries than the box holds: readers that trust the count walk
  // past the box end, which is exactly the "incomplete index" case.
  put_be32(data, payload + 4, static_cast<uint32_t>(new_count));
  return write_all(path, data, err);
}

bool corrupt_sample(const std::filesystem::path& path, int64_t sample_index,
                    int stream_ordinal, int64_t& damage_offset, int64_t& damage_size,
                    std::string& err) {
  FileStructure fs;
  Error e;
  if (!read_mp4_structure(path.string(), fs, e)) {
    err = e.detail;
    return false;
  }
  int64_t seen = 0;
  const TrackStructure* target = nullptr;
  for (const auto& t : fs.tracks) {
    if (!t.is_video()) continue;
    if (seen++ == stream_ordinal) {
      target = &t;
      break;
    }
  }
  if (!target || sample_index < 0 ||
      sample_index >= static_cast<int64_t>(target->samples.size())) {
    err = "sample index out of range";
    return false;
  }
  const SampleRef& ref = target->samples[static_cast<size_t>(sample_index)];
  std::string data;
  if (!read_all(path, data, err)) return false;
  if (ref.offset < 0 || ref.offset + ref.size > static_cast<int64_t>(data.size())) {
    err = "sample lies outside the file";
    return false;
  }
  // Overwrite the sample payload with a byte pattern the MPEG-4 and AAC
  // decoders reject.  The sample header (first 8 bytes) is kept so the
  // container stays walkable; the damage is inside the packet body.
  damage_offset = ref.offset;
  damage_size = ref.size;
  for (int64_t i = std::min<int64_t>(8, ref.size); i < ref.size; ++i) {
    data[static_cast<size_t>(ref.offset + i)] = char(0xA5);
  }
  return write_all(path, data, err);
}

bool patch_fragment_tfdt(const std::filesystem::path& path,
                         const std::vector<int64_t>& per_tfdt_values, std::string& err) {
  std::string data;
  if (!read_all(path, data, err)) return false;
  size_t at = 0;
  size_t found = 0;
  while (true) {
    const size_t idx = data.find("tfdt", at);
    if (idx == std::string::npos) break;
    if (found < per_tfdt_values.size()) {
      // tfdt payload: version(1) flags(3) baseMediaDecodeTime(4 or 8)
      const size_t payload = idx + 4;
      const uint8_t version = static_cast<uint8_t>(data[payload]);
      if (version == 1) {
        put_be64(data, payload + 4, static_cast<uint64_t>(per_tfdt_values[found]));
      } else {
        put_be32(data, payload + 4, static_cast<uint32_t>(per_tfdt_values[found]));
      }
    }
    ++found;
    at = idx + 4;
  }
  if (found == 0) {
    err = "no tfdt found";
    return false;
  }
  return write_all(path, data, err);
}

int64_t count_top_level_box(const std::filesystem::path& path, const std::string& type) {
  FileStructure fs;
  Error e;
  if (!read_mp4_structure(path.string(), fs, e)) return -1;
  int64_t n = 0;
  for (const auto& b : fs.boxes) {
    if (b.type_str() == type) ++n;
  }
  return n;
}

// ---- fixture set --------------------------------------------------------

namespace {

// The generated timestamps in fragmented fixtures are relative to the
// stream time base, so wrap values are computed from the actual time base.
bool make_fragmented_source(const std::string& ffmpeg_binary, const std::string& path,
                            int seconds, std::string& err) {
  const std::string dur = std::to_string(seconds);
  std::vector<std::string> args = {
      "-hide_banner", "-loglevel", "error", "-y",
      "-f", "lavfi", "-i", "testsrc2=size=128x96:rate=10:duration=" + dur,
      "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=" + dur,
      "-c:v", "mpeg4", "-g", "10", "-q:v", "5",
      "-c:a", "aac", "-b:a", "64k",
      "-movflags", "+empty_moov+frag_keyframe+skip_trailer",
      "-f", "mp4", path,
  };
  return run_process(ffmpeg_binary, args, err);
}

}  // namespace

FixtureSet generate_fixtures(const std::filesystem::path& work_dir,
                             const std::string& ffmpeg_binary) {
  FixtureSet set;
  set.work_dir = work_dir;
  set.input_dir = work_dir / "input";
  set.output_dir = work_dir / "output";
  set.checkpoint_dir = work_dir / "checkpoints";
  std::error_code ec;
  std::filesystem::create_directories(set.input_dir, ec);
  std::filesystem::create_directories(set.output_dir, ec);
  std::filesystem::create_directories(set.checkpoint_dir, ec);
  if (ec) {
    set.ok = false;
    set.error = "cannot create work directories: " + ec.message();
    return set;
  }
  const std::string base = (work_dir / "source.mp4").string();
  std::string err;
  if (!encode_source(ffmpeg_binary, base, 3, true, err)) {
    set.ok = false;
    set.error = "ffmpeg encode failed: " + err;
    return set;
  }
  // 1. Clean faststart file.
  {
    const auto p = set.input_dir / "01_clean.mp4";
    std::filesystem::copy_file(base, p, std::filesystem::copy_options::overwrite_existing);
    set.files.push_back(make_info(p, FixtureKind::Clean, true,
                                  "baseline: moov in front, clean packets"));
  }
  // 2. Truncated tail: cut into the last mdat.
  {
    const auto p = set.input_dir / "02_truncated_tail.mp4";
    std::filesystem::copy_file(base, p, std::filesystem::copy_options::overwrite_existing);
    const int64_t size = file_size_bytes(p);
    const int64_t keep = size - std::max<int64_t>(64, size / 200);
    if (!truncate_file(p, keep, err)) {
      set.ok = false;
      set.error = "truncate fixture: " + err;
      return set;
    }
    auto info = make_info(p, FixtureKind::TruncatedTail, true,
                          "tail cut inside the last mdat; complete packets are kept");
    info.damage_offset = keep;
    info.damage_size = size - keep;
    set.files.push_back(info);
  }
  // 3. Corrupt packet inside a video sample.
  {
    const auto p = set.input_dir / "03_corrupt_packet.mp4";
    std::filesystem::copy_file(base, p, std::filesystem::copy_options::overwrite_existing);
    int64_t off = -1, len = 0;
    if (!corrupt_sample(p, 4, 0, off, len, err)) {
      set.ok = false;
      set.error = "corrupt fixture: " + err;
      return set;
    }
    auto info = make_info(p, FixtureKind::CorruptPacket, true,
                          "byte pattern overwritten inside one video sample");
    info.damage_offset = off;
    info.damage_size = len;
    set.files.push_back(info);
  }
  // 4. Index entry out of range.
  {
    const auto p = set.input_dir / "04_index_out_of_range.mp4";
    std::filesystem::copy_file(base, p, std::filesystem::copy_options::overwrite_existing);
    if (!patch_stco_entry(p, 5, 0x7FFFFFF0, err)) {
      set.ok = false;
      set.error = "stco fixture: " + err;
      return set;
    }
    auto info = make_info(p, FixtureKind::IndexOutOfRange, true,
                          "chunk 6 offset points past EOF; index must be rebuilt from present samples");
    set.files.push_back(info);
  }
  // 5. Incomplete index: stco claims more entries than the box holds.
  {
    const auto p = set.input_dir / "05_incomplete_index.mp4";
    std::filesystem::copy_file(base, p, std::filesystem::copy_options::overwrite_existing);
    std::string data;
    if (!read_all(p, data, err)) {
      set.ok = false;
      set.error = err;
      return set;
    }
    size_t payload = 0, len = 0, hdr = 0;
    if (find_nested(data, "moov/trak/mdia/minf/stbl/stco", payload, len, hdr)) {
      const uint32_t count = (uint32_t(uint8_t(data[payload + 4])) << 24) |
                             (uint32_t(uint8_t(data[payload + 5])) << 16) |
                             (uint32_t(uint8_t(data[payload + 6])) << 8) |
                             uint8_t(data[payload + 7]);
      if (!patch_stco_entry_count(p, int64_t(count) * 4 + 10, err)) {
        set.ok = false;
        set.error = err;
        return set;
      }
    }
    set.files.push_back(make_info(p, FixtureKind::IncompleteIndex, true,
                                  "stco entry count exceeds the box; only the present entries are used"));
  }
  // 6. moov at the end (no faststart) and 7. no moov at all.
  {
    const std::string tail_src = (work_dir / "source_tail.mp4").string();
    std::vector<std::string> args = {
        "-hide_banner", "-loglevel", "error", "-y",
        "-f", "lavfi", "-i", "testsrc2=size=128x96:rate=10:duration=3",
        "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=3",
        "-c:v", "mpeg4", "-g", "10", "-q:v", "5", "-c:a", "aac", "-b:a", "64k",
        tail_src,
    };
    if (!run_process(ffmpeg_binary, args, err)) {
      set.ok = false;
      set.error = "moov-at-end encode failed: " + err;
      return set;
    }
    const auto p = set.input_dir / "06_moov_at_end.mov";
    std::filesystem::copy_file(tail_src, p, std::filesystem::copy_options::overwrite_existing);
    set.files.push_back(make_info(p, FixtureKind::MoovAtEnd, true,
                                  "moov after mdat; output must place the index first"));
    const auto q = set.input_dir / "07_no_moov.mp4";
    if (!drop_moov(tail_src, q, err)) {
      set.ok = false;
      set.error = "drop moov failed: " + err;
      return set;
    }
    set.files.push_back(make_info(q, FixtureKind::NoMoov, false,
                                  "moov removed entirely; must fail with a localised reason"));
    // 8. Truncated moov: cut into the trailing moov.
    const auto r = set.input_dir / "08_truncated_moov.mp4";
    std::filesystem::copy_file(tail_src, r, std::filesystem::copy_options::overwrite_existing);
    const int64_t size = file_size_bytes(r);
    FileStructure fs;
    Error e;
    read_mp4_structure(r.string(), fs, e);
    const int64_t keep = fs.moov_offset > 0 ? fs.moov_offset + (size - fs.moov_offset) / 3
                                            : size - 64;
    if (!truncate_file(r, keep, err)) {
      set.ok = false;
      set.error = "truncated moov fixture: " + err;
      return set;
    }
    auto info = make_info(r, FixtureKind::TruncatedMoov, false,
                          "moov cut mid-table; stream parameters are incomplete");
    info.damage_offset = keep;
    set.files.push_back(info);
  }
  // 9. Timestamp wrap and 10. backwards jump, from fragmented sources.
  {
    const std::string frag = (work_dir / "source_frag.mp4").string();
    if (!make_fragmented_source(ffmpeg_binary, frag, 4, err)) {
      set.ok = false;
      set.error = "fragmented encode failed: " + err;
      return set;
    }
    const auto wrap = set.input_dir / "09_timestamp_wrap.mp4";
    const auto back = set.input_dir / "10_backwards.mp4";
    std::filesystem::copy_file(frag, wrap, std::filesystem::copy_options::overwrite_existing);
    std::filesystem::copy_file(frag, back, std::filesystem::copy_options::overwrite_existing);
    // Fragment 2 starts near the top of the 32-bit range and the fragment's
    // own decode time continues from there past 2^32, so readers see the
    // rollover mid-fragment.  The patched base is offset so the first frame
    // of the fragment lands just below 2^32.
    if (!patch_fragment_tfdt(wrap, {0, 0, 0xFFFFFF00LL - 3 * 1024, 0xFFFFFF00LL - 3 * 1024}, err)) {
      set.ok = false;
      set.error = "tfdt wrap patch failed: " + err;
      return set;
    }
    // Both tracks restart 1 s into the file: a backwards jump far beyond any
    // frame jitter and far below the rollover threshold.
    if (!patch_fragment_tfdt(back, {0, 0, 5000, 24000}, err)) {
      set.ok = false;
      set.error = "tfdt backwards patch failed: " + err;
      return set;
    }
    set.files.push_back(make_info(wrap, FixtureKind::TimestampWrap, true,
                                  "second fragment wraps the 32-bit decode time; unwrapped"));
    // 4435 ticks backward in a 1/10240 base (~0.43 s): far beyond two frame
    // durations and far below the rollover threshold, so it cannot be repaired
    // without guessing.  The asset must fail with the exact packet offset.
    set.files.push_back(make_info(back, FixtureKind::BackwardsTimestamp, false,
                                  "second fragment restarts 4435 ticks back; neither a "
                                  "rollover nor a bounded glitch"));
  }
  // 11. Zero length input.
  {
    const auto p = set.input_dir / "11_zero_length.mp4";
    std::string err2;
    if (!write_all(p, "", err2)) {
      set.ok = false;
      set.error = err2;
      return set;
    }
    set.files.push_back(make_info(p, FixtureKind::ZeroLength, false,
                                  "zero bytes: no header, no offset to report"));
  }
  // 12. Stale checkpoint: a checkpoint whose input checksum cannot match.
  {
    const auto p = set.input_dir / "12_stale_checkpoint.mp4";
    std::filesystem::copy_file(base, p, std::filesystem::copy_options::overwrite_existing);
    auto info = make_info(p, FixtureKind::StaleCheckpoint, true,
                          "valid media with a pre-seeded stale checkpoint");
    set.files.push_back(info);

    ResumeStore store(set.checkpoint_dir);
    Config cfg;
    Checkpoint cp;
    cp.stage = JobStage::Streaming;
    cp.input_path = p.string();
    cp.input_sha256 = std::string(64, '0');  // deliberately wrong
    cp.input_size = info.input_size;
    cp.config_digest = mediaqc::config_digest(cfg.summary());
    cp.output_path = (set.output_dir / p.filename()).string();
    cp.input_offset = std::max<int64_t>(0, info.input_size / 2);
    cp.packets_written = 1;
    std::string serr;
    if (!store.save(cp, serr)) {
      set.ok = false;
      set.error = "cannot seed stale checkpoint: " + serr;
      return set;
    }
  }
  return set;
}

}  // namespace mediaqc
