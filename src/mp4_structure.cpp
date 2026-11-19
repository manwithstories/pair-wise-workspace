#include "mp4_structure.hpp"

#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <cstring>
#include <map>

namespace mediaqc {
namespace {

constexpr int64_t kMaxIndexEntries = 20'000'000;  // bounds memory on absurd stsz
constexpr int kMaxDepth = 8;

uint32_t rd32(const uint8_t* p) {
  return (uint32_t(p[0]) << 24) | (uint32_t(p[1]) << 16) | (uint32_t(p[2]) << 8) | p[3];
}
uint64_t rd64(const uint8_t* p) {
  return (uint64_t(rd32(p)) << 32) | rd32(p + 4);
}

bool read_at(int fd, int64_t offset, void* buf, size_t len) {
  size_t done = 0;
  while (done < len) {
    const ssize_t n = ::pread(fd, static_cast<char*>(buf) + done, len - done,
                              static_cast<off_t>(offset + done));
    if (n <= 0) return false;
    done += static_cast<size_t>(n);
  }
  return true;
}

// ---- box header ---------------------------------------------------------

bool parse_box_header(const uint8_t* p, int64_t avail, int64_t base, Box& b,
                      int64_t& header_len) {
  if (avail < 8) return false;
  const uint32_t size32 = rd32(p);
  std::memcpy(b.type, p + 4, 4);
  b.type[4] = 0;
  b.offset = base;
  header_len = 8;
  int64_t size = size32;
  if (size32 == 1) {
    if (avail < 16) return false;
    b.size_64 = true;
    size = static_cast<int64_t>(rd64(p + 8));
    header_len = 16;
  } else if (size32 == 0) {
    size = -1;  // extends to EOF
  }
  b.size = size;
  return true;
}

// Container boxes whose first payload byte begins a child box.
bool is_container(const char* t) {
  static const char* kContainers[] = {"moov", "trak", "mdia", "minf", "stbl",
                                      "mvex", "moof", "traf", "edts", "dinf",
                                      "udta", "meta", "wave"};
  for (const char* c : kContainers) {
    if (std::strcmp(c, t) == 0) return true;
  }
  return false;
}

}  // namespace

void walk_boxes(const uint8_t* data, int64_t len, int64_t base, int depth,
                std::vector<Box>& out) {
  (void)depth;
  int64_t off = 0;
  while (off + 8 <= len) {
    Box b;
    int64_t header_len = 8;
    if (!parse_box_header(data + off, len - off, base + off, b, header_len)) break;
    b.depth = depth;
    if (b.size < 0) {
      b.size = len - off;  // size 0: to end of the enclosing box
    } else if (off + b.size > len) {
      // The box is cut short by the enclosing buffer; clamp so callers can
      // still inspect the parts that are present.
      b.truncated = true;
      b.size = len - off;
    }
    out.push_back(b);
    if (b.size <= header_len) break;  // guard against zero/negative progress
    if (is_container(b.type) && depth < kMaxDepth) {
      walk_boxes(data + off + header_len, b.size - header_len, base + off + header_len,
                 depth + 1, out);
    }
    off += b.size;
  }
}

namespace {

// ---- specific box parsers -----------------------------------------------

void parse_mdhd(const uint8_t* p, int64_t len, TrackStructure& t) {
  if (len < 4) return;
  const uint8_t version = p[0];
  const int64_t need = version == 1 ? 4 + 4 + 8 + 8 : 4 + 4 + 4 + 4;
  if (len < need) return;
  t.timescale = version == 1 ? rd32(p + 20) : rd32(p + 12);
}

void parse_hdlr(const uint8_t* p, int64_t len, TrackStructure& t) {
  if (len < 12) return;
  std::memcpy(t.handler, p + 8, 4);
  t.handler[4] = 0;
}

void parse_tkhd(const uint8_t* p, int64_t len, TrackStructure& t) {
  if (len < 4) return;
  const uint8_t version = p[0];
  if (version == 1) {
    if (len < 24) return;
    t.track_id = rd32(p + 20);
  } else {
    if (len < 16) return;
    t.track_id = rd32(p + 12);
  }
}

void parse_stco(const uint8_t* p, int64_t len, bool co64, TrackStructure& t) {
  if (len < 8) return;
  const uint32_t count = rd32(p + 4);
  if (count > kMaxIndexEntries) return;
  const int64_t width = co64 ? 8 : 4;
  const int64_t need = 8 + int64_t(count) * width;
  t.has_stco = true;
  t.co64 = co64;
  // A box that claims more entries than it holds is an incomplete index: keep
  // the entries that are physically present and record the shortfall so the
  // scanner reports an out-of-range table rather than silently trusting it.
  const int64_t present = std::max<int64_t>(0, (len - 8) / width);
  const bool truncated = need > len;
  const int64_t usable = truncated ? std::min<int64_t>(present, count) : int64_t(count);
  t.stco_truncated = truncated;
  t.stco_missing_entries = truncated ? int64_t(count) - usable : 0;
  t.nb_chunks = usable;
  t.chunk_offsets.reserve(static_cast<size_t>(usable));
  for (int64_t i = 0; i < usable; ++i) {
    t.chunk_offsets.push_back(co64 ? static_cast<int64_t>(rd64(p + 8 + i * 8))
                                   : static_cast<int64_t>(rd32(p + 8 + i * 4)));
  }
}

void parse_stsz(const uint8_t* p, int64_t len, TrackStructure& t) {
  if (len < 12) return;
  const uint32_t sample_size = rd32(p + 4);
  const uint32_t count = rd32(p + 8);
  if (count > kMaxIndexEntries) return;
  t.nb_samples = count;
  t.sample_sizes.resize(count);
  if (sample_size != 0) {
    std::fill(t.sample_sizes.begin(), t.sample_sizes.end(),
              static_cast<int32_t>(sample_size));
    return;
  }
  const int64_t need = 12 + int64_t(count) * 4;
  if (need > len) {
    // The table itself is cut off; keep what is present, the rest is damage.
    const uint32_t have = uint32_t(std::max<int64_t>(0, (len - 12) / 4));
    for (uint32_t i = 0; i < have; ++i) {
      t.sample_sizes[i] = static_cast<int32_t>(rd32(p + 12 + i * 4));
    }
    for (uint32_t i = have; i < count; ++i) t.sample_sizes[i] = 0;
    return;
  }
  for (uint32_t i = 0; i < count; ++i) {
    t.sample_sizes[i] = static_cast<int32_t>(rd32(p + 12 + i * 4));
  }
}

void parse_stsc(const uint8_t* p, int64_t len, TrackStructure& t) {
  if (len < 8) return;
  const uint32_t count = rd32(p + 4);
  const int64_t need = 8 + int64_t(count) * 12;
  if (need > len) return;
  t.runs.reserve(count);
  for (uint32_t i = 0; i < count; ++i) {
    ChunkRun run;
    run.first_chunk = rd32(p + 8 + i * 12);
    run.samples_per_chunk = static_cast<int32_t>(rd32(p + 12 + i * 12));
    t.runs.push_back(run);
  }
}

void parse_stts(const uint8_t* p, int64_t len, TrackStructure& t) {
  if (len < 8) return;
  const uint32_t count = rd32(p + 4);
  const int64_t need = 8 + int64_t(count) * 8;
  if (need > len) return;
  t.sample_dts.reserve(t.nb_samples);
  int64_t dts = 0;
  int64_t written = 0;
  for (uint32_t i = 0; i < count && written < t.nb_samples; ++i) {
    const uint32_t n = rd32(p + 8 + i * 8);
    const uint32_t delta = rd32(p + 12 + i * 8);
    for (uint32_t k = 0; k < n && written < t.nb_samples; ++k) {
      t.sample_dts.push_back(dts);
      dts += delta;
      ++written;
    }
  }
}

void parse_stsd_handler(const uint8_t* p, int64_t len) {
  // stsd: full box(4) entry_count(4) then sample entries.  The handler already
  // identifies the track kind; we only need to know whether the entry parses
  // as a visual sample entry for the index reconstruction report.
  if (len < 8) return;
  const uint32_t entries = rd32(p + 4);
  if (entries == 0) return;
  Box b;
  int64_t header_len = 0;
  if (!parse_box_header(p + 8, len - 8, 0, b, header_len)) return;
  (void)b;
}

}  // namespace

bool read_mp4_structure(const std::string& path, FileStructure& out, Error& err) {
  out = FileStructure{};
  err = Error{};
  err.file = path;
  err.stage = Stage::Structure;

  struct stat st{};
  if (::stat(path.c_str(), &st) != 0) {
    err.code = Code::HeaderUnparsable;
    err.detail = std::string("stat failed: ") + std::strerror(errno);
    err.action = "reject input";
    return false;
  }
  const int fd = ::open(path.c_str(), O_RDONLY);
  if (fd < 0) {
    err.code = Code::HeaderUnparsable;
    err.detail = std::string("open failed: ") + std::strerror(errno);
    err.action = "reject input";
    return false;
  }
  out.file_size = static_cast<int64_t>(st.st_size);
  out.last_confirmed_offset = out.file_size;

  // ---- top level boxes --------------------------------------------------
  int64_t off = 0;
  int track_cursor = -1;
  while (off + 8 <= out.file_size) {
    uint8_t hdr[16];
    const int64_t avail = out.file_size - off;
    if (!read_at(fd, off, hdr, avail >= 16 ? 16 : size_t(avail))) break;
    Box b;
    int64_t header_len = 8;
    if (!parse_box_header(hdr, avail >= 16 ? 16 : avail, off, b, header_len)) break;
    int64_t box_end = b.size < 0 ? out.file_size : off + b.size;
    if (b.size > 0 && box_end > out.file_size) {
      b.truncated = true;
      out.truncated = true;
      // Every byte up to EOF is present; the first byte that is missing (and
      // therefore the first we cannot confirm) is the end of the file.
      if (out.first_truncated_offset < 0) {
        out.first_truncated_offset = out.file_size;
        out.truncated_box = b.type_str();
      }
      out.last_confirmed_offset = out.file_size;
    }
    out.boxes.push_back(b);
    if (std::strcmp(b.type, "ftyp") == 0) out.parsed_ftyp = true;
    if (std::strcmp(b.type, "mdat") == 0) {
      MdatBox m;
      m.offset = off;
      m.header_size = header_len;
      m.declared_size = b.size;
      m.data_offset = off + header_len;
      m.truncated = b.truncated;
      m.data_size = b.truncated ? std::max<int64_t>(0, out.file_size - m.data_offset) : b.size - header_len;
      out.mdats.push_back(m);
    }
    if (std::strcmp(b.type, "moof") == 0) {
      out.fragmented = true;
      if (out.moof_offset < 0) out.moof_offset = off;
    }
    if (std::strcmp(b.type, "mfra") == 0 && out.mfra_offset < 0) out.mfra_offset = off;

    if (std::strcmp(b.type, "moov") == 0) {
      out.has_moov = true;
      out.moov_offset = off;
      out.moov_size = b.size;
      if (b.truncated) {
        out.moov_truncated = true;
        // The moov extends past EOF: the first missing byte is EOF itself, the
        // moov payload was readable up to there.
        if (out.first_truncated_offset < 0) out.first_truncated_offset = out.file_size;
        if (out.truncated_box.empty()) out.truncated_box = "moov";
      }
      const int64_t payload_len = std::min<int64_t>(b.size - header_len,
                                                    out.file_size - (off + header_len));
      if (payload_len > 0) {
        std::vector<uint8_t> payload(static_cast<size_t>(payload_len));
        read_at(fd, off + header_len, payload.data(), payload.size());
        std::vector<Box> inner;
        walk_boxes(payload.data(), payload_len, off + header_len, 1, inner);
        for (const auto& ib : inner) {
          if (std::strcmp(ib.type, "trak") != 0) continue;
          TrackStructure track;
          track.moov_offset = off;
          std::vector<Box> fields;
          walk_boxes(payload.data() + (ib.offset - (off + header_len)),
                     ib.size, ib.offset, 2, fields);
          for (const auto& f : fields) {
            const int64_t rel = f.offset - ib.offset;
            const uint8_t* fp = payload.data() + (f.offset - (off + header_len));
            const int64_t flen = std::min<int64_t>(f.size, payload_len - (f.offset - (off + header_len)));
            if (flen <= 0) continue;
            (void)rel;
            if (std::strcmp(f.type, "tkhd") == 0) {
              parse_tkhd(fp + 8, flen - 8, track);
            } else if (std::strcmp(f.type, "mdhd") == 0) {
              parse_mdhd(fp + 8, flen - 8, track);
            } else if (std::strcmp(f.type, "hdlr") == 0) {
              parse_hdlr(fp + 8, flen - 8, track);
            } else if (std::strcmp(f.type, "stco") == 0) {
              track.stco_offset = f.offset;
              parse_stco(fp + 8, flen - 8, false, track);
            } else if (std::strcmp(f.type, "co64") == 0) {
              track.stco_offset = f.offset;
              parse_stco(fp + 8, flen - 8, true, track);
            } else if (std::strcmp(f.type, "stsz") == 0) {
              track.stsz_offset = f.offset;
              parse_stsz(fp + 8, flen - 8, track);
            } else if (std::strcmp(f.type, "stsc") == 0) {
              parse_stsc(fp + 8, flen - 8, track);
            } else if (std::strcmp(f.type, "stts") == 0) {
              parse_stts(fp + 8, flen - 8, track);
            } else if (std::strcmp(f.type, "stbl") == 0) {
              track.has_stbl = true;
              track.stbl_offset = f.offset;
            } else if (std::strcmp(f.type, "stsd") == 0) {
              parse_stsd_handler(fp + 8, flen - 8);
            }
          }
          if (track.has_stbl) {
            build_sample_offsets(track, out.file_size);
            out.tracks.push_back(std::move(track));
          }
        }
      }
    }
    (void)track_cursor;
    if (b.size < 0) break;          // zero-sized box consumes the rest
    if (b.size < header_len) break; // malformed: cannot make progress
    off = box_end;
  }
  // A trailing fragment that holds a partial box header (fewer than 8 bytes)
  // is itself damage: report the offset where the walk stopped.
  if (out.file_size - off > 0 && out.file_size - off < 8 && !out.truncated) {
    out.truncated = true;
    out.truncated_box = "partial_trailer";
    out.first_truncated_offset = off;
    out.last_confirmed_offset = off;
  }

  if (!out.has_moov) {
    out.moov_truncated = false;
  }
  out.moov_after_mdat = moov_follows_media(out);
  if (!out.has_moov) {
    out.last_confirmed_offset = off;  // boxes parsed cleanly up to here
  }
  ::close(fd);
  return true;
}

void build_sample_offsets(TrackStructure& track, int64_t file_size) {
  track.samples.clear();
  track.index_entries_out_of_range = 0;
  track.first_bad_sample = -1;
  track.first_bad_offset = -1;
  track.first_bad_end = -1;
  track.first_bad_entry_offset = -1;
  track.first_bad_target = -1;
  track.samples_past_eof = 0;
  track.first_past_eof_sample = -1;
  if (!track.has_stco || track.runs.empty() || track.sample_sizes.empty()) return;

  // Walk chunks in order, advancing the stsc run as chunk numbers pass each
  // run's first_chunk.  Runs are sorted by first_chunk as stored.
  int64_t sample = 0;
  size_t run = 0;
  int64_t chunk_bytes_used = 0;
  const int64_t chunks = std::min<int64_t>(track.nb_chunks,
                                           static_cast<int64_t>(track.chunk_offsets.size()));
  int64_t chunk = 1;
  for (; chunk <= chunks; ++chunk) {
    uint32_t samples_per_chunk = 1;
    for (size_t i = 0; i < track.runs.size(); ++i) {
      if (track.runs[i].first_chunk <= chunk) samples_per_chunk = static_cast<uint32_t>(
          std::max<int32_t>(0, track.runs[i].samples_per_chunk));
      else break;
    }
    (void)run;
    const int64_t chunk_offset = track.chunk_offsets[static_cast<size_t>(chunk - 1)];
    // Byte position of this chunk's stco/co64 entry: box header (8) plus
    // version/flags (4) and entry count (4) precede the entries.
    const int64_t entry_offset =
        track.stco_offset >= 0
            ? track.stco_offset + 16 + int64_t(chunk - 1) * (track.co64 ? 8 : 4)
            : -1;
    chunk_bytes_used = 0;
    for (uint32_t k = 0; k < samples_per_chunk && sample < track.nb_samples; ++k, ++sample) {
      const int64_t size = track.sample_sizes[static_cast<size_t>(sample)];
      SampleRef ref;
      ref.offset = chunk_offset + chunk_bytes_used;
      ref.size = size;
      track.samples.push_back(ref);
      chunk_bytes_used += size;
      // A chunk offset that points past EOF *for the whole chunk* (i.e. the
      // chunk starts at or beyond EOF) is a bad index entry.  A chunk that
      // starts inside the file but whose samples run past EOF is a tail cut.
      const bool chunk_starts_after_eof = chunk_offset >= file_size;
      if (ref.offset < 0 || ref.size < 0 || chunk_starts_after_eof) {
        // The chunk offset itself is unusable: a corrupt index.
        track.index_entries_out_of_range++;
        if (track.first_bad_sample < 0) {
          track.first_bad_sample = sample;
          track.first_bad_offset = ref.offset;
          track.first_bad_end = ref.offset + std::max<int64_t>(0, ref.size);
          track.first_bad_entry_offset = entry_offset;
          track.first_bad_target = chunk_offset;
        }
      } else if (ref.offset + ref.size > file_size) {
        // The entry is valid but the file ends inside this sample: a
        // truncated tail.  The damage is at EOF, not in the index.
        track.samples_past_eof++;
        if (track.first_past_eof_sample < 0) track.first_past_eof_sample = sample;
      }
    }
  }
  // Chunk offset entries that the stco box promised but did not hold leave
  // the tail of the sample table unaddressable; that is index damage too.
  if (track.stco_missing_entries > 0) {
    track.index_entries_out_of_range += track.stco_missing_entries;
    if (track.first_bad_sample < 0) {
      track.first_bad_sample = sample;
      track.first_bad_offset = track.stco_offset + 16 + int64_t(track.nb_chunks) *
                                                        (track.co64 ? 8 : 4);
      track.first_bad_end = track.first_bad_offset + (track.co64 ? 8 : 4);
      track.first_bad_entry_offset = track.first_bad_offset;
      track.first_bad_target = 0;
    }
  }
  // A sample table that promises more samples than stsz provides is damage.
  if (sample != track.nb_samples && track.first_bad_sample < 0) {
    track.first_bad_sample = sample;
    track.first_bad_offset = track.samples.empty() ? 0 : track.samples.back().offset;
    track.index_entries_out_of_range++;
  }
}

bool moov_follows_media(const FileStructure& s) {
  bool saw_mdat = false;
  for (const auto& b : s.boxes) {
    if (std::strcmp(b.type, "mdat") == 0) saw_mdat = true;
    if (std::strcmp(b.type, "moov") == 0) return saw_mdat;
  }
  return false;
}

}  // namespace mediaqc
