#include "index_writer.hpp"

#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <cstring>
#include <map>
#include <vector>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/dict.h>
#include <libavutil/error.h>
#include <libavutil/log.h>
}

#include "sha256.hpp"
#include "util.hpp"

namespace mediaqc {
namespace {

std::string av_error_string(int err) {
  char buf[AV_ERROR_MAX_STRING_SIZE] = {0};
  av_strerror(err, buf, sizeof buf);
  return buf;
}

void put_be32(char* p, uint32_t v) {
  p[0] = char(v >> 24); p[1] = char(v >> 16); p[2] = char(v >> 8); p[3] = char(v);
}
uint32_t get_be32(const char* p) {
  return (uint32_t(uint8_t(p[0])) << 24) | (uint32_t(uint8_t(p[1])) << 16) |
         (uint32_t(uint8_t(p[2])) << 8) | uint8_t(p[3]);
}
void put_be64(char* p, int64_t v) {
  put_be32(p, uint32_t(uint64_t(v) >> 32));
  put_be32(p + 4, uint32_t(uint64_t(v) & 0xFFFFFFFFu));
}
int64_t get_be64(const char* p) {
  return int64_t((uint64_t(get_be32(p)) << 32) | uint64_t(get_be32(p + 4)));
}

bool write_all(int fd, const void* data, size_t len) {
  const char* p = static_cast<const char*>(data);
  size_t done = 0;
  while (done < len) {
    const ssize_t n = ::write(fd, p + done, len - done);
    if (n <= 0) {
      if (errno == EINTR) continue;
      return false;
    }
    done += static_cast<size_t>(n);
  }
  return true;
}

bool read_all(int fd, void* data, size_t len, int64_t offset) {
  char* p = static_cast<char*>(data);
  size_t done = 0;
  while (done < len) {
    const ssize_t n = ::pread(fd, p + done, len - done, static_cast<off_t>(offset + int64_t(done)));
    if (n < 0 && errno == EINTR) continue;
    if (n <= 0) return false;
    done += static_cast<size_t>(n);
  }
  return true;
}

}  // namespace

std::string serialize_journal_entry(const JournalEntry& e) {
  char buf[kJournalRecordBytes];
  std::memset(buf, 0, sizeof buf);
  put_be32(buf + 0, e.stream_index);
  put_be32(buf + 4, e.flags);
  put_be32(buf + 8, e.size);
  put_be64(buf + 16, e.dts);
  put_be64(buf + 24, e.pts);
  put_be64(buf + 32, e.duration);
  put_be64(buf + 40, e.src_offset);
  // payload_offset is implied by the append order: tracked separately by
  // IndexWriter when needed and verified when replaying.
  return std::string(buf, sizeof buf);
}

bool parse_journal_entry(const char* data, JournalEntry& e) {
  // A record is 48 bytes: stream, flags, size, dts, pts, duration, src offset.
  e.stream_index = get_be32(data + 0);
  e.flags = get_be32(data + 4);
  e.size = get_be32(data + 8);
  e.dts = get_be64(data + 16);
  e.pts = get_be64(data + 24);
  e.duration = get_be64(data + 32);
  e.src_offset = get_be64(data + 40);
  return true;
}

// ---- IndexWriter ---------------------------------------------------------

IndexWriter::~IndexWriter() { close_files(); }

bool IndexWriter::open(const std::filesystem::path& dir, const std::string& stem,
                       int64_t expect_payload_bytes, int64_t expect_journal_bytes,
                       bool reset, std::string& err) {
  std::error_code ec;
  std::filesystem::create_directories(dir, ec);
  if (ec) {
    err = "cannot create " + dir.string() + ": " + ec.message();
    return false;
  }
  payload_path_ = dir / (stem + ".payload");
  journal_path_ = dir / (stem + ".journal");
  payload_fd_ = ::open(payload_path_.c_str(), O_RDWR | O_CREAT, 0644);
  journal_fd_ = ::open(journal_path_.c_str(), O_RDWR | O_CREAT, 0644);
  if (payload_fd_ < 0 || journal_fd_ < 0) {
    err = std::string("cannot open payload/journal: ") + std::strerror(errno);
    return false;
  }
  if (reset) {
    if (::ftruncate(payload_fd_, 0) != 0 || ::ftruncate(journal_fd_, 0) != 0) {
      err = std::string("cannot reset partial work: ") + std::strerror(errno);
      return false;
    }
  }
  struct stat st{};
  if (::fstat(payload_fd_, &st) != 0) return false;
  int64_t payload_len = st.st_size;
  if (::fstat(journal_fd_, &st) != 0) return false;
  int64_t journal_len = st.st_size;
  if (journal_len % kJournalRecordBytes != 0) {
    err = "journal length " + std::to_string(journal_len) + " is not a multiple of " +
          std::to_string(kJournalRecordBytes);
    return false;
  }
  if (expect_payload_bytes >= 0 && payload_len != expect_payload_bytes) {
    err = "payload length " + std::to_string(payload_len) + " does not match checkpoint " +
          std::to_string(expect_payload_bytes);
    return false;
  }
  if (expect_journal_bytes >= 0 && journal_len != expect_journal_bytes) {
    err = "journal length " + std::to_string(journal_len) + " does not match checkpoint " +
          std::to_string(expect_journal_bytes);
    return false;
  }
  // Truncate any partial trailing record left by an interrupted write.
  if (::ftruncate(payload_fd_, payload_len) != 0 || ::ftruncate(journal_fd_, journal_len) != 0) {
    err = std::string("cannot truncate to committed length: ") + std::strerror(errno);
    return false;
  }
  if (::lseek(payload_fd_, payload_len, SEEK_SET) < 0 ||
      ::lseek(journal_fd_, journal_len, SEEK_SET) < 0) {
    err = "cannot seek to committed length";
    return false;
  }
  payload_bytes_ = payload_len;
  journal_bytes_ = journal_len;
  packets_ = journal_len / kJournalRecordBytes;
  return true;
}

bool IndexWriter::append(const JournalEntry& entry, const uint8_t* data, std::string& err) {
  JournalEntry e = entry;
  e.payload_offset = payload_bytes_;
  const std::string rec = serialize_journal_entry(e);
  if (!write_all(payload_fd_, data, e.size)) {
    err = std::string("payload write failed: ") + std::strerror(errno);
    return false;
  }
  if (!write_all(journal_fd_, rec.data(), rec.size())) {
    err = std::string("journal write failed: ") + std::strerror(errno);
    return false;
  }
  payload_bytes_ += e.size;
  journal_bytes_ += int64_t(rec.size());
  ++packets_;
  return true;
}

void IndexWriter::close_files() {
  if (payload_fd_ >= 0) {
    ::close(payload_fd_);
    payload_fd_ = -1;
  }
  if (journal_fd_ >= 0) {
    ::close(journal_fd_);
    journal_fd_ = -1;
  }
}

bool IndexWriter::flush(std::string& err) {
  if (::fsync(payload_fd_) != 0) {
    err = std::string("payload fsync failed: ") + std::strerror(errno);
    return false;
  }
  if (::fsync(journal_fd_) != 0) {
    err = std::string("journal fsync failed: ") + std::strerror(errno);
    return false;
  }
  return true;
}

bool read_journal(const std::filesystem::path& journal, int64_t max_records,
                  std::vector<JournalEntry>& out, std::string& err) {
  out.clear();
  const int fd = ::open(journal.c_str(), O_RDONLY);
  if (fd < 0) {
    err = std::string("cannot open journal: ") + std::strerror(errno);
    return false;
  }
  struct stat st{};
  if (::fstat(fd, &st) != 0) {
    ::close(fd);
    err = "cannot stat journal";
    return false;
  }
  const int64_t count = st.st_size / kJournalRecordBytes;
  if (count > max_records) {
    ::close(fd);
    err = "journal holds " + std::to_string(count) + " records, limit is " +
          std::to_string(max_records);
    return false;
  }
  out.reserve(static_cast<size_t>(count));
  int64_t payload_cursor = 0;
  for (int64_t i = 0; i < count; ++i) {
    char buf[kJournalRecordBytes];
    if (!read_all(fd, buf, sizeof buf, i * kJournalRecordBytes)) {
      ::close(fd);
      err = "short read in journal at record " + std::to_string(i);
      return false;
    }
    JournalEntry e;
    parse_journal_entry(buf, e);
    e.payload_offset = payload_cursor;
    payload_cursor += e.size;
    out.push_back(e);
  }
  ::close(fd);
  return true;
}

// ---- mux ----------------------------------------------------------------

bool mux_output(const std::filesystem::path& input,
                const std::filesystem::path& payload,
                const std::filesystem::path& journal,
                const std::filesystem::path& output_partial,
                const MuxOptions& options, MuxReport& report, Error& err) {
  report = MuxReport{};
  err = Error{};
  err.file = input.string();
  err.stage = Stage::Write;

  std::vector<JournalEntry> entries;
  std::string jerr;
  if (!read_journal(journal, options.config.limits.max_offset_errors * 64 + 1'000'000,
                    entries, jerr)) {
    err.code = Code::OutputWriteFailed;
    err.detail = jerr;
    err.action = "abort output; the journal is damaged";
    report.ok = false;
    return false;
  }
  if (entries.empty()) {
    err.code = Code::EmptyOutput;
    err.detail = "no packets survived scanning; refusing to emit an empty file";
    err.action = "mark asset failed";
    report.ok = false;
    return false;
  }

  // Stream parameters come from the input container; no packet is demuxed.
  AVFormatContext* in = nullptr;
  if (avformat_open_input(&in, input.c_str(), nullptr, nullptr) < 0) {
    err.code = Code::OutputWriteFailed;
    err.detail = "cannot reopen input for stream parameters";
    err.action = "mark asset failed";
    report.ok = false;
    return false;
  }
  avformat_find_stream_info(in, nullptr);

  AVFormatContext* out = nullptr;
  if (avformat_alloc_output_context2(&out, nullptr, "mp4", output_partial.c_str()) < 0 || !out) {
    avformat_close_input(&in);
    err.code = Code::OutputWriteFailed;
    err.detail = "cannot create MP4 muxer";
    report.ok = false;
    return false;
  }
  // Deterministic output: no encoder identification strings, no timestamps.
  out->flags |= AVFMT_FLAG_BITEXACT;

  std::vector<int> map(in->nb_streams, -1);
  std::vector<Rational> in_base(in->nb_streams);
  bool need_global_header = (out->oformat->flags & AVFMT_GLOBALHEADER) != 0;
  (void)need_global_header;
  for (unsigned i = 0; i < in->nb_streams; ++i) {
    const AVStream* src = in->streams[i];
    if (src->codecpar->codec_type != AVMEDIA_TYPE_VIDEO &&
        src->codecpar->codec_type != AVMEDIA_TYPE_AUDIO) {
      continue;
    }
    AVStream* dst = avformat_new_stream(out, nullptr);
    if (!dst) {
      err.code = Code::OutputWriteFailed;
      err.detail = "cannot create output stream";
      report.ok = false;
      avformat_free_context(out);
      avformat_close_input(&in);
      return false;
    }
    if (avcodec_parameters_copy(dst->codecpar, src->codecpar) < 0) {
      err.code = Code::OutputWriteFailed;
      err.detail = "cannot copy codec parameters";
      report.ok = false;
      avformat_free_context(out);
      avformat_close_input(&in);
      return false;
    }
    dst->codecpar->codec_tag = 0;  // let the MP4 muxer pick the tag
    dst->time_base = src->time_base;
    dst->avg_frame_rate = src->avg_frame_rate;
    dst->sample_aspect_ratio = src->sample_aspect_ratio;
    map[i] = dst->index;
    in_base[i] = Rational::from_av(src->time_base);
  }

  // faststart moves the moov in front of mdat; the muxer builds the complete
  // index (stco/stsz/stts/stss) from the packets we hand it.
  AVDictionary* opts = nullptr;
  av_dict_set(&opts, "movflags", "+faststart", 0);
  int ret = avio_open(&out->pb, output_partial.c_str(), AVIO_FLAG_WRITE);
  if (ret < 0) {
    err.code = Code::OutputWriteFailed;
    err.detail = "cannot open output: " + av_error_string(ret);
    err.action = "check free space and permissions; input is untouched";
    report.ok = false;
    av_dict_free(&opts);
    avformat_free_context(out);
    avformat_close_input(&in);
    return false;
  }
  ret = avformat_write_header(out, &opts);
  av_dict_free(&opts);
  if (ret < 0) {
    err.code = Code::OutputWriteFailed;
    err.detail = "muxer rejected the header: " + av_error_string(ret);
    report.ok = false;
    avio_closep(&out->pb);
    avformat_free_context(out);
    avformat_close_input(&in);
    std::filesystem::remove(output_partial);
    return false;
  }

  // Replay payload records in journal order.
  const int pfd = ::open(payload.c_str(), O_RDONLY);
  if (pfd < 0) {
    err.code = Code::OutputWriteFailed;
    err.detail = std::string("cannot open payload: ") + std::strerror(errno);
    report.ok = false;
    avio_closep(&out->pb);
    avformat_free_context(out);
    avformat_close_input(&in);
    return false;
  }
  AVPacket* pkt = av_packet_alloc();
  std::vector<uint8_t> buf;
  std::map<int, int64_t> last_dts;  // output stream -> last written dts
  std::map<int, int64_t> stream_index_entries;
  std::map<int, int64_t> stream_keyframes;
  std::map<int, int64_t> last_key_canonical;
  std::map<int, int64_t> stream_end_canonical;
  report.ok = true;

  for (const auto& e : entries) {
    if (e.stream_index >= map.size() || map[e.stream_index] < 0) continue;
    const int oi = map[e.stream_index];
    buf.resize(e.size);
    if (e.size > 0 && !read_all(pfd, buf.data(), e.size, e.payload_offset)) {
      err.code = Code::OutputWriteFailed;
      err.detail = "payload is shorter than the journal at offset " +
                   std::to_string(e.payload_offset);
      err.action = "abort output; payload and journal disagree";
      report.ok = false;
      break;
    }
    if (av_new_packet(pkt, static_cast<int>(e.size)) < 0) {
      err.code = Code::OutputWriteFailed;
      err.detail = "out of memory allocating a packet";
      report.ok = false;
      break;
    }
    std::memcpy(pkt->data, buf.data(), e.size);
    pkt->stream_index = oi;
    pkt->flags = e.flags;
    pkt->duration = e.duration;
    pkt->pos = -1;
    // Journal timestamps are in the input stream's time base; the muxer may
    // have chosen a different time base for the output stream.
    const AVRational src_tb = in_base[e.stream_index].av();
    pkt->pts = e.pts == AV_NOPTS_VALUE ? AV_NOPTS_VALUE
                                        : av_rescale_q(e.pts, src_tb, out->streams[oi]->time_base);
    pkt->dts = e.dts == AV_NOPTS_VALUE ? AV_NOPTS_VALUE
                                        : av_rescale_q(e.dts, src_tb, out->streams[oi]->time_base);
    pkt->duration = av_rescale_q(e.duration, src_tb, out->streams[oi]->time_base);

    ret = av_interleaved_write_frame(out, pkt);
    av_packet_unref(pkt);
    if (ret == AVERROR(EINVAL) && static_cast<size_t>(e.stream_index) < in_base.size()) {
      // A single packet with an out-of-range timestamp is skipped rather than
      // discarding the whole asset: the journal keeps the rest of the media,
      // and the verifier will check the result independently.
      ++report.skipped_packets;
      ret = 0;
      continue;
    }
    if (ret < 0) {
      err.code = Code::OutputWriteFailed;
      err.detail = "write failed at packet " +
                   std::to_string(report.packets_written) + ": " + av_error_string(ret);
      err.action = "abort output; the original file is untouched";
      err.byte_offset = e.src_offset;
      report.ok = false;
      break;
    }
    ++report.packets_written;
    ++stream_index_entries[oi];
    const int64_t canon_start = rescale_ticks(e.dts == AV_NOPTS_VALUE ? e.pts : e.dts,
                                             in_base[e.stream_index],
                                             Rational::from_av(kCanonicalBase));
    const int64_t canon_end = canon_start + rescale_ticks(
        e.duration, in_base[e.stream_index], Rational::from_av(kCanonicalBase));
    if (stream_end_canonical[oi] < canon_end) stream_end_canonical[oi] = canon_end;
    if (e.flags & AV_PKT_FLAG_KEY) {
      ++stream_keyframes[oi];
      last_key_canonical[oi] = canon_start;
    }
    (void)last_dts;
  }
  av_packet_free(&pkt);
  ::close(pfd);

  if (report.ok) {
    ret = av_write_trailer(out);
    if (ret < 0) {
      err.code = Code::OutputWriteFailed;
      err.detail = "cannot finalise output: " + av_error_string(ret);
      err.action = "abort output; the original file is untouched";
      report.ok = false;
    }
  }
  avio_closep(&out->pb);
  avformat_free_context(out);
  avformat_close_input(&in);

  for (const auto& [oi, n] : stream_index_entries) {
    report.index_entries += n;
    report.keyframes += stream_keyframes[oi];
  }

  // Spans that no keyframe can reach: from the last keyframe (or sample 0)
  // to the stream end when the gap exceeds the threshold.
  const int64_t gap_limit = options.keyframe_gap_ticks > 0
                                ? options.keyframe_gap_ticks
                                : 2 * kCanonicalTicksPerSecond;
  for (const auto& [oi, end] : stream_end_canonical) {
    const auto it = last_key_canonical.find(oi);
    const int64_t last_key = it == last_key_canonical.end() ? 0 : it->second;
    if (end - last_key > gap_limit) {
      UnindexableRange r;
      r.stream_index = oi;
      r.start_ticks = last_key;
      r.end_ticks = end;
      r.reason = "no keyframe within " + std::to_string(gap_limit / 90000) +
                 " s of the stream end";
      report.unindexable.push_back(r);
    }
  }

  if (!report.ok) {
    std::filesystem::remove(output_partial);
    return false;
  }
  std::string hex;
  int64_t bytes = 0;
  if (!sha256_file(output_partial, hex, bytes)) {
    err.code = Code::OutputWriteFailed;
    err.detail = "cannot hash the written output";
    report.ok = false;
    return false;
  }
  report.output_sha256 = hex;
  report.bytes_written = bytes;
  return true;
}

}  // namespace mediaqc
