#include "verifier.hpp"

#include <algorithm>
#include <cstdarg>
#include <cstring>
#include <cstdlib>
#include <map>
#include <set>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/error.h>
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

struct DecodeLogContext {
  int pending_errors = 0;
  std::string last_message;
};

// Set by the log callback and read after each decode step.  The callback has
// no per-context opaque in this FFmpeg API, so a single static instance is
// used; decode is strictly serial inside verify_output.
DecodeLogContext g_log;

void decode_log_callback(void* /*opaque*/, int level, const char* fmt, va_list ap) {
  if (level > AV_LOG_ERROR) return;
  DecodeLogContext* ctx = &g_log;
  char buf[512];
  std::vsnprintf(buf, sizeof buf, fmt, ap);
  char* nl = std::strchr(buf, '\n');
  if (nl) *nl = 0;
  ctx->pending_errors++;
  ctx->last_message = buf;
}

void add_error(VerificationReport& r, const Error& e) {
  r.errors.push_back(e);
  if (e.offset_known &&
      (r.first_error_offset < 0 || e.byte_offset < r.first_error_offset)) {
    r.first_error_offset = e.byte_offset;
    r.offset_error_bytes = e.offset_uncertainty();
  }
}

// Maps an output stream ordinal (0-based among verified streams) and an
// output packet index to the journal record that produced it.
const JournalEntry* journal_for(const std::vector<JournalEntry>& entries,
                                const std::vector<int>& out_stream_of_demux,
                                int out_stream_index, int64_t packet_ordinal) {
  int64_t seen = 0;
  for (const auto& e : entries) {
    if (e.stream_index >= out_stream_of_demux.size()) continue;
    if (out_stream_of_demux[e.stream_index] != out_stream_index) continue;
    if (seen == packet_ordinal) return &e;
    seen++;
  }
  return nullptr;
}

}  // namespace

bool verify_output(const std::filesystem::path& output,
                   const std::filesystem::path& journal,
                   const std::filesystem::path& /*input*/, const Config& config,
                   VerificationReport& report, Error& fatal) {
  report = VerificationReport{};
  fatal = Error{};
  if (std::getenv("MEDIAQC_TRACE")) std::fprintf(stderr, "[verify] begin\n");
  fatal.file = output.string();
  fatal.stage = Stage::Verify;

  std::string hex;
  int64_t bytes = 0;
  if (!sha256_file(output, hex, bytes)) {
    fatal.code = Code::OutputVerifyFailed;
    fatal.detail = "cannot read output for hashing";
    fatal.action = "reject the output";
    report.ok = false;
    return false;
  }
  report.output_sha256 = hex;
  report.output_bytes = bytes;
  if (bytes == 0) {
    fatal.code = Code::EmptyOutput;
    fatal.detail = "output is zero bytes";
    fatal.action = "reject the output";
    report.ok = false;
    return false;
  }

  if (std::getenv("MEDIAQC_TRACE")) std::fprintf(stderr, "[verify] hashed\n");
  std::vector<JournalEntry> entries;
  if (!journal.empty()) {
    std::string jerr;
    if (!read_journal(journal, config.limits.max_offset_errors * 64 + 1'000'000, entries,
                      jerr)) {
      fatal.code = Code::OutputVerifyFailed;
      fatal.detail = "cannot read journal: " + jerr;
      report.ok = false;
      return false;
    }
  }

  AVFormatContext* ctx = nullptr;
  int ret = avformat_open_input(&ctx, output.c_str(), nullptr, nullptr);
  if (ret < 0) {
    fatal.code = Code::OutputVerifyFailed;
    fatal.detail = "output does not open: " + av_error_string(ret);
    fatal.action = "reject the output; the previous file is kept";
    report.ok = false;
    return false;
  }
  ret = avformat_find_stream_info(ctx, nullptr);
  if (ret < 0) {
    fatal.code = Code::OutputVerifyFailed;
    fatal.detail = "output stream info is unreadable: " + av_error_string(ret);
    report.ok = false;
    avformat_close_input(&ctx);
    return false;
  }

  // Index entries are counted on their own AVFormatContext: seeking below
  // repositions this one, and the count must reflect the whole file.
  int64_t total_index = 0;
  int64_t checked = 0;
  int64_t bad = 0;
  int64_t index_present = 0;
  {
    AVFormatContext* idx_ctx = nullptr;
    if (avformat_open_input(&idx_ctx, output.c_str(), nullptr, nullptr) >= 0) {
      if (avformat_find_stream_info(idx_ctx, nullptr) < 0) {
        fatal.code = Code::OutputVerifyFailed;
        fatal.detail = "output stream info is unreadable on the index pass";
        report.ok = false;
        avformat_close_input(&idx_ctx);
        avformat_close_input(&ctx);
        return false;
      }
      for (unsigned i = 0; i < idx_ctx->nb_streams; ++i) {
        AVStream* st = idx_ctx->streams[i];
        const int n = avformat_index_get_entries_count(st);
        total_index += n;
        if (n > 0) index_present = 1;
        for (int k = 0; k < n; ++k) {
          const AVIndexEntry* e = avformat_index_get_entry(st, k);
          if (!e) {
            ++bad;
            continue;
          }
          ++checked;
          if (e->pos < 0 || e->pos >= bytes) {
            ++bad;
            Error err;
            err.file = output.string();
            err.stage = Stage::Verify;
            err.severity = Severity::Fatal;
            err.code = Code::IndexEntryOutOfRange;
            err.track = static_cast<int>(i);
            err.set_range(e->pos, e->pos + 4);
            err.detail = "index entry " + std::to_string(k) + " points at output byte " +
                         std::to_string(e->pos) + " beyond " + std::to_string(bytes);
            err.action = "reject the output; the index is unusable";
            add_error(report, err);
          }
        }
      }
      avformat_close_input(&idx_ctx);
    }
  }
  report.index_entries = total_index;
  report.index_entries_checked = checked;
  report.index_errors = bad;
  report.index_present = index_present != 0;
  if (!report.index_present) {
    fatal.code = Code::OutputVerifyFailed;
    fatal.detail = "output has no index entries; random access is impossible";
    fatal.action = "reject the output";
    report.ok = false;
    avformat_close_input(&ctx);
    return false;
  }
  if (bad > 0) report.ok = false;

  if (std::getenv("MEDIAQC_TRACE")) std::fprintf(stderr, "[verify] index checked\n");
  // ---- seek reachability --------------------------------------------------
  int64_t video_stream = -1;
  for (unsigned i = 0; i < ctx->nb_streams; ++i) {
    if (ctx->streams[i]->codecpar->codec_type == AVMEDIA_TYPE_VIDEO) {
      video_stream = static_cast<int64_t>(i);
      break;
    }
  }
  if (video_stream >= 0) {
    AVStream* st = ctx->streams[video_stream];
    const int64_t duration_canonical =
        st->duration > 0
            ? rescale_ticks(st->duration, Rational::from_av(st->time_base),
                            Rational::from_av(kCanonicalBase))
            : 0;
    const Rational tb = Rational::from_av(st->time_base);
    const int64_t targets[] = {0, duration_canonical / 4, duration_canonical / 2,
                               (duration_canonical * 3) / 4, duration_canonical};
    for (int64_t t : targets) {
      if (duration_canonical <= 0) break;
      SeekProbe probe;
      probe.stream_index = static_cast<int>(video_stream);
      probe.target_ticks = t;
      const int64_t tick = rescale_ticks(t, Rational::from_av(kCanonicalBase), tb);
      const int sret = av_seek_frame(ctx, static_cast<int>(video_stream), tick,
                                     AVSEEK_FLAG_BACKWARD);
      if (sret < 0) {
        probe.note = "seek failed: " + av_error_string(sret);
        report.seek_failures++;
        Error err;
        err.file = output.string();
        err.stage = Stage::Verify;
        err.severity = Severity::Fatal;
        err.code = Code::UnindexableRange;
        err.track = static_cast<int>(video_stream);
        err.offset_known = false;
        err.detail = "cannot seek to canonical tick " + std::to_string(t) + ": " + probe.note;
        err.action = "reject the output; random access is required";
        add_error(report, err);
        report.ok = false;
        report.probes.push_back(probe);
        continue;
      }
      AVPacket* pkt = av_packet_alloc();
      int64_t landed_dts = AV_NOPTS_VALUE;
      int64_t landed_pts = AV_NOPTS_VALUE;
      int64_t landed_pos = -1;
      bool key = false;
      while (av_read_frame(ctx, pkt) >= 0) {
        if (pkt->stream_index == video_stream) {
          landed_dts = rescale_ticks(pkt->dts, tb, Rational::from_av(kCanonicalBase));
          landed_pts = rescale_ticks(pkt->pts, tb, Rational::from_av(kCanonicalBase));
          landed_pos = pkt->pos;
          key = (pkt->flags & AV_PKT_FLAG_KEY) != 0;
          av_packet_unref(pkt);
          break;
        }
        av_packet_unref(pkt);
      }
      av_packet_free(&pkt);
      probe.landed_dts = landed_dts;
      probe.landed_pts = landed_pts;
      probe.landed_offset = landed_pos;
      probe.keyframe = key;
      probe.reachable = landed_dts != AV_NOPTS_VALUE && key &&
                        landed_dts <= rescale_ticks(t, Rational::from_av(kCanonicalBase),
                                                    Rational::from_av(kCanonicalBase));
      if (landed_dts != AV_NOPTS_VALUE && landed_dts > t) {
        probe.reachable = false;
        probe.note = "seek landed after the requested time";
      }
      if (!probe.reachable) {
        report.seek_failures++;
        Error err;
        err.file = output.string();
        err.stage = Stage::Verify;
        err.severity = Severity::Fatal;
        err.code = Code::UnindexableRange;
        err.track = static_cast<int>(video_stream);
        err.set_range(landed_pos >= 0 ? landed_pos : 0, 0);
        err.detail = "seek to canonical tick " + std::to_string(t) +
                     " did not land on a keyframe at or before it";
        err.action = "reject the output; the index does not reach this point";
        add_error(report, err);
        report.ok = false;
      }
      report.probes.push_back(probe);
      ++report.seek_probes;
    }
    av_seek_frame(ctx, -1, 0, AVSEEK_FLAG_BACKWARD);
  }

  if (std::getenv("MEDIAQC_TRACE")) std::fprintf(stderr, "[verify] seeks done\n");
  // ---- decode every stream and check monotonic DTS ------------------------
  std::map<int, AVCodecContext*> decoders;
  std::vector<int> out_stream_of_demux(ctx->nb_streams, -1);
  std::map<int, int> demux_by_out;
  for (unsigned i = 0; i < ctx->nb_streams; ++i) {
    const AVCodec* codec = avcodec_find_decoder(ctx->streams[i]->codecpar->codec_id);
    if (!codec) continue;
    AVCodecContext* cc = avcodec_alloc_context3(codec);
    if (!cc) continue;
    if (avcodec_parameters_to_context(cc, ctx->streams[i]->codecpar) < 0) {
      avcodec_free_context(&cc);
      continue;
    }
    cc->thread_count = 1;  // deterministic, and errors are attributed in order
    if (avcodec_open2(cc, codec, nullptr) < 0) {
      avcodec_free_context(&cc);
      continue;
    }
    decoders[i] = cc;
    demux_by_out[static_cast<int>(i)] = static_cast<int>(i);
    out_stream_of_demux[i] = static_cast<int>(i);
  }

  std::map<int, int64_t> last_dts;
  std::map<int, int64_t> packet_ordinal;
  std::map<int, int64_t> frames;
  AVPacket* pkt = av_packet_alloc();
  AVFrame* frame = av_frame_alloc();
  int decode_errors = 0;
  int monotonic_errors = 0;

  // FFmpeg's log callback has no per-decoder opaque in this API, so the
  // verifier routes messages through a single static context while it decodes
  // one packet at a time on one thread.
  g_log = DecodeLogContext{};
  av_log_set_level(AV_LOG_ERROR);
  av_log_set_callback(decode_log_callback);

  if (std::getenv("MEDIAQC_TRACE")) std::fprintf(stderr, "[verify] decoders open\n");
  while (av_read_frame(ctx, pkt) >= 0) {
    const int si = pkt->stream_index;
    if (std::getenv("MEDIAQC_TRACE")) std::fprintf(stderr, "[verify] pkt %lld stream %d\n", (long long)report.packets, si);
    ++report.packets;
    const int64_t ordinal = packet_ordinal[si]++;
    auto dit = decoders.find(si);
    if (dit == decoders.end()) {
      av_packet_unref(pkt);
      continue;
    }
    if (pkt->dts != AV_NOPTS_VALUE) {
      auto ld = last_dts.find(si);
      if (ld != last_dts.end() && pkt->dts < ld->second) {
        ++monotonic_errors;
        Error err;
        err.file = output.string();
        err.stage = Stage::Verify;
        err.severity = Severity::Fatal;
        err.code = Code::TimestampBackwards;
        err.track = si;
        const JournalEntry* je = journal_for(entries, out_stream_of_demux, si, ordinal);
        if (je && je->src_offset >= 0) {
          err.set_range(je->src_offset, je->src_offset + je->size);
          err.detail = "output DTS went backwards (" + std::to_string(pkt->dts) +
                       " < " + std::to_string(ld->second) + ")";
          err.action = "reject the output";
        } else {
          err.offset_known = false;
          err.detail = "output DTS went backwards with no input mapping";
        }
        add_error(report, err);
        report.ok = false;
      }
      last_dts[si] = pkt->dts;
    }
    if (pkt->flags & AV_PKT_FLAG_KEY) ++report.keyframes;

    // Decode and attribute any decoder error to the packet in flight.
    g_log.pending_errors = 0;
    g_log.last_message.clear();
    int send = avcodec_send_packet(dit->second, pkt);
    while (avcodec_receive_frame(dit->second, frame) >= 0) {
      frames[si]++;
      report.frames_decoded++;
      if (g_log.pending_errors > 0 && decode_errors < config.limits.max_decode_errors) {
        Error err;
        err.file = output.string();
        err.stage = Stage::Verify;
        err.severity = Severity::Recovered;
        err.code = Code::DecodeError;
        err.track = si;
        const JournalEntry* je = journal_for(entries, out_stream_of_demux, si, ordinal);
        if (je && je->src_offset >= 0) {
          // The decoder does not expose an intra-packet position, so the
          // whole packet is the uncertainty span: exactly one packet wide.
          err.set_range(je->src_offset, je->src_offset + je->size);
          err.detail = "decoder reported: " + g_log.last_message +
                       "; input range covers the packet that produced the frame";
          err.action = "report the input byte range; the fault is inside one packet";
        } else {
          err.offset_known = false;
          err.detail = "decoder reported: " + g_log.last_message;
        }
        add_error(report, err);
        decode_errors++;
        g_log.pending_errors = 0;
      }
      if (send < 0) break;
    }
    if (send < 0 && decode_errors < config.limits.max_decode_errors) {
      Error err;
      err.file = output.string();
      err.stage = Stage::Verify;
      err.severity = Severity::Recovered;
      err.code = Code::DecodeError;
      err.track = si;
      const JournalEntry* je = journal_for(entries, out_stream_of_demux, si, ordinal);
      if (je && je->src_offset >= 0) {
        err.set_range(je->src_offset, je->src_offset + je->size);
        err.detail = "decoder rejected a packet: " + av_error_string(send);
        err.action = "report the input byte range; the fault is inside one packet";
      } else {
        err.offset_known = false;
        err.detail = "decoder rejected a packet: " + av_error_string(send);
      }
      add_error(report, err);
      decode_errors++;
    }
    av_packet_unref(pkt);
  }
  av_log_set_callback(av_log_default_callback);
  // The static context must not keep a pointer to a destroyed report; its
  // contents are plain data, so only reset the counters.
  g_log = DecodeLogContext{};

  // Flush decoders so trailing frames are counted and attributed.
  for (auto& [si, cc] : decoders) {
    g_log.pending_errors = 0;
    g_log.last_message.clear();
    avcodec_send_packet(cc, nullptr);
    while (avcodec_receive_frame(cc, frame) >= 0) {
      frames[si]++;
      report.frames_decoded++;
    }
  }
  for (auto& [si, cc] : decoders) avcodec_free_context(&cc);
  av_packet_free(&pkt);
  av_frame_free(&frame);
  avformat_close_input(&ctx);

  report.decode_errors = decode_errors;
  report.monotonic_errors = monotonic_errors;

  // Every packet written should be indexable; a large shortfall means the
  // muxer dropped data.
  if (report.index_entries + 4 < report.packets) {
    Error err;
    err.file = output.string();
    err.stage = Stage::Verify;
    err.severity = Severity::Fatal;
    err.code = Code::OutputVerifyFailed;
    err.offset_known = false;
    err.detail = "index has " + std::to_string(report.index_entries) +
                 " entries for " + std::to_string(report.packets) + " packets";
    err.action = "reject the output";
    add_error(report, err);
    report.ok = false;
  }

  // Decode failures in the output do not by themselves invalidate the file:
  // they describe damage that came from the input and are reported with the
  // input byte range.  They do, however, fail verification when the asset was
  // supposed to be clean, which the pipeline decides from the input report.
  if (report.seek_failures > 0 || report.index_errors > 0) report.ok = false;
  fatal = Error{};
  return report.ok;
}

}  // namespace mediaqc
