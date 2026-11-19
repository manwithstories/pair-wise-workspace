#include "scanner.hpp"

#include <algorithm>
#include <cstdlib>
#include <cstring>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/error.h>
#include <libavutil/log.h>
}

#include "sha256.hpp"
#include "util.hpp"

namespace mediaqc {
namespace {

// FFmpeg demuxers are tolerant of truncation: they return the bytes that are
// present for the final packet instead of failing.  mediaqc therefore trusts
// its own sample table for byte accounting and treats a packet whose declared
// span crosses EOF as damaged.
std::string av_error_string(int err) {
  char buf[AV_ERROR_MAX_STRING_SIZE] = {0};
  av_strerror(err, buf, sizeof buf);
  return buf;
}

// Finds the container track id for a demuxer stream index, using the tkhd
// ids recovered by the structure walker (they are in mux order).
uint32_t track_id_for(const FileStructure& fs, unsigned stream_index) {
  if (stream_index < fs.tracks.size()) return fs.tracks[stream_index].track_id;
  return 0;
}

const TrackStructure* container_track_for(const FileStructure& fs,
                                          uint32_t track_id) {
  if (track_id == 0) return nullptr;
  for (const auto& t : fs.tracks) {
    if (t.track_id == track_id) return &t;
  }
  return nullptr;
}

}  // namespace

const char* input_kind_name(InputKind k) {
  switch (k) {
    case InputKind::Ok: return "ok";
    case InputKind::Empty: return "empty";
    case InputKind::Unparsable: return "unparsable";
    case InputKind::NoMoov: return "no_moov";
    case InputKind::TruncatedMoov: return "truncated_moov";
    case InputKind::TooLarge: return "too_large";
    case InputKind::TooManyTracks: return "too_many_tracks";
    case InputKind::Unsupported: return "unsupported";
  }
  return "unknown";
}

Scanner::~Scanner() {
  if (ctx_) avformat_close_input(&ctx_);
}

void Scanner::record(ScanOutcome& out, const Error& e) {
  out.errors.push_back(e);
  if (e.offset_known &&
      (out.first_damaged_offset < 0 || e.byte_offset < out.first_damaged_offset)) {
    out.first_damaged_offset = e.byte_offset;
    out.offset_uncertainty = e.offset_uncertainty();
  }
}

bool Scanner::open(const std::string& path, const Limits& limits, ScanOutcome& out) {
  path_ = path;
  out = ScanOutcome{};
  // FFmpeg's demuxer diagnostics are duplicated by the structured errors this
  // scanner reports; keep them out of the terminal unless requested.
  if (std::getenv("MEDIAQC_FFMPEG_LOG") == nullptr) av_log_set_level(AV_LOG_QUIET);

  Error base;
  base.file = path;
  base.stage = Stage::Open;
  base.action = "reject input";

  // ---- size and emptiness -------------------------------------------------
  out.file_size = file_size_bytes(path);
  if (out.file_size < 0) {
    out.kind = InputKind::Unparsable;
    out.fatal = true;
    base.code = Code::HeaderUnparsable;
    base.detail = "cannot stat input file";
    out.fatal_error = base;
    return false;
  }
  if (out.file_size > limits.max_input_bytes) {
    out.kind = InputKind::TooLarge;
    out.fatal = true;
    base.code = Code::FileTooLarge;
    base.detail = "input is " + std::to_string(out.file_size) + " bytes, limit is " +
                  std::to_string(limits.max_input_bytes);
    out.fatal_error = base;
    return false;
  }
  if (out.file_size == 0) {
    out.kind = InputKind::Empty;
    out.fatal = true;
    base.code = Code::FileEmpty;
    base.byte_offset = 0;
    base.last_confirmed = 0;
    base.detail = "input has zero bytes; no header, no media, no offset to report";
    base.action = "mark asset failed; nothing to repair";
    out.fatal_error = base;
    return false;
  }

  // ---- independent structure pass ----------------------------------------
  Error struct_err;
  if (!read_mp4_structure(path, out.structure, struct_err)) {
    out.kind = InputKind::Unparsable;
    out.fatal = true;
    base.code = Code::HeaderUnparsable;
    base.detail = struct_err.detail;
    out.fatal_error = base;
    return false;
  }
  out.fragmented = out.structure.fragmented;
  out.truncated_tail = out.structure.truncated;
  {
    std::string hex;
    int64_t bytes = 0;
    if (!sha256_file(path, hex, bytes)) {
      out.kind = InputKind::Unparsable;
      out.fatal = true;
      base.code = Code::HeaderUnparsable;
      base.detail = "cannot read input for hashing";
      out.fatal_error = base;
      return false;
    }
    out.input_sha256 = hex;
  }

  const bool has_usable_moov = out.structure.has_moov && !out.structure.moov_truncated;
  if (!out.structure.has_moov) {
    // No moov at all: no stream parameters exist, so nothing can be decoded.
    // (A moov at the end of a truncated file is the "truncated moov" case.)
    out.kind = InputKind::NoMoov;
    out.fatal = true;
    base.code = Code::NoMoov;
    base.stage = Stage::Structure;
    // The nearest offset that can be confirmed is where the box walk ended:
    // every byte before it was parsed as a valid top-level box, and no moov
    // was among them.  The byte immediately after is the first one we cannot
    // account for.
    const int64_t confirmed = out.structure.last_confirmed_offset >= 0
                                  ? out.structure.last_confirmed_offset
                                  : out.file_size;
    base.byte_offset = confirmed;
    base.byte_offset_end = 0;
    base.last_confirmed = confirmed;
    base.detail = "no moov box in " + std::to_string(out.file_size) +
                  " bytes; every top-level box up to offset " +
                  std::to_string(confirmed) + " parsed, none was moov";
    base.action = "mark asset failed; a moov is required for stream parameters";
    out.fatal_error = base;
    return false;
  }
  if (out.structure.moov_truncated) {
    out.kind = InputKind::TruncatedMoov;
    out.fatal = true;
    base.code = Code::MoovTruncated;
    base.stage = Stage::Structure;
    base.byte_offset = out.structure.first_truncated_offset >= 0
                           ? out.structure.first_truncated_offset
                           : out.file_size;
    base.last_confirmed = out.structure.last_confirmed_offset;
    base.detail = "moov box at " + std::to_string(out.structure.moov_offset) +
                  " declares " + std::to_string(out.structure.moov_size) +
                  " bytes but the file ends at " + std::to_string(out.file_size);
    base.action = "mark asset failed; stream tables are incomplete";
    out.fatal_error = base;
    return false;
  }
  (void)has_usable_moov;

  // ---- libavformat --------------------------------------------------------
  int ret = avformat_open_input(&ctx_, path.c_str(), nullptr, nullptr);
  if (ret < 0) {
    out.kind = InputKind::Unparsable;
    out.fatal = true;
    base.code = Code::HeaderUnparsable;
    base.stage = Stage::Structure;
    base.detail = "libavformat could not open the container: " + av_error_string(ret);
    base.byte_offset = out.structure.file_size;
    base.last_confirmed = out.structure.last_confirmed_offset;
    out.fatal_error = base;
    return false;
  }
  ret = avformat_find_stream_info(ctx_, nullptr);
  if (ret < 0) {
    out.kind = InputKind::Unparsable;
    out.fatal = true;
    base.code = Code::HeaderUnparsable;
    base.stage = Stage::Structure;
    base.detail = "libavformat could not read stream info: " + av_error_string(ret);
    base.byte_offset = out.structure.file_size;
    base.last_confirmed = out.structure.last_confirmed_offset;
    out.fatal_error = base;
    return false;
  }
  if (ctx_->nb_streams > static_cast<unsigned>(limits.max_tracks)) {
    out.kind = InputKind::TooManyTracks;
    out.fatal = true;
    base.code = Code::TrackLimitExceeded;
    base.detail = "container has " + std::to_string(ctx_->nb_streams) +
                  " streams, limit is " + std::to_string(limits.max_tracks);
    out.fatal_error = base;
    return false;
  }

  limits_ = limits;
  collect_tracks(out);

  // A stream whose codec parameters are incomplete (no dimensions for video,
  // no sample rate for audio) cannot be re-muxed into MP4 and its frames
  // cannot be decoded, so its damage cannot be localised.  Report it as an
  // out-of-range index: the entry that should describe the first sample is
  // the byte the container failed to give us.
  for (auto& spec : out.tracks) {
    const bool missing_video = spec.media_type == AVMEDIA_TYPE_VIDEO &&
                               (spec.width <= 0 || spec.height <= 0);
    const bool missing_audio = spec.media_type == AVMEDIA_TYPE_AUDIO &&
                               spec.sample_rate <= 0;
    if (!missing_video && !missing_audio) continue;
    const TrackStructure* ct = container_track_for(out.structure, spec.track_id);
    Error e;
    e.file = path;
    e.stage = Stage::Structure;
    e.severity = Severity::Fatal;
    e.code = Code::IndexEntryOutOfRange;
    e.track = static_cast<int>(spec.track_id);
    e.stream_index = spec.stream_index;
    const int64_t at = ct && ct->first_bad_entry_offset >= 0
                           ? ct->first_bad_entry_offset
                           : (ct && ct->stco_offset >= 0 ? ct->stco_offset
                                                         : out.structure.moov_offset);
    e.set_range(at, at + 4);
    e.last_confirmed = out.structure.last_confirmed_offset;
    e.detail = "sample table points past EOF so codec parameters are incomplete (" +
               std::string(missing_video ? "video dimensions" : "audio sample rate") +
               " missing)";
    e.action = "rebuild the sample table from the present sample data";
    out.fatal = true;
    out.fatal_error = e;
  }
  validate_structure(out);
  return !out.fatal;
}

void Scanner::collect_tracks(ScanOutcome& out) {
  tracks_.clear();
  out.tracks.clear();
  for (unsigned i = 0; i < ctx_->nb_streams; ++i) {
    AVStream* st = ctx_->streams[i];
    if (st->codecpar->codec_type != AVMEDIA_TYPE_VIDEO &&
        st->codecpar->codec_type != AVMEDIA_TYPE_AUDIO) {
      continue;
    }
    TrackSpec spec;
    spec.stream_index = static_cast<int>(i);
    spec.track_id = track_id_for(out.structure, i);
    spec.media_type = st->codecpar->codec_type;
    spec.codec_name = avcodec_get_name(st->codecpar->codec_id);
    spec.time_base = Rational::from_av(st->time_base);
    spec.frame_rate = Rational::from_av(st->avg_frame_rate);
    spec.width = st->codecpar->width;
    spec.height = st->codecpar->height;
    spec.sample_rate = st->codecpar->sample_rate;
    spec.channels = st->codecpar->ch_layout.nb_channels;
    spec.container_duration = st->duration >= 0 ? st->duration : 0;

    const AVCodec* codec = avcodec_find_decoder(st->codecpar->codec_id);
    spec.decodable = codec != nullptr;

    if (const TrackStructure* ct = container_track_for(out.structure, spec.track_id)) {
      spec.container_samples = ct->nb_samples;
      spec.first_bad_sample = ct->first_bad_sample;
      spec.first_bad_offset = ct->first_bad_offset;
      spec.samples_out_of_range = ct->index_entries_out_of_range;
      spec.samples_past_eof = ct->samples_past_eof;
      spec.first_past_eof_sample = ct->first_past_eof_sample;
      spec.index_entries_present = ct->has_stco && ct->nb_samples > 0;
      spec.index_incomplete = ct->first_bad_sample >= 0;
      // A sample that merely extends past EOF is a truncated tail, reported
      // separately; it does not make the index unusable.
    } else if (out.fragmented) {
      // Fragmented files carry their sample tables in moof/traf; libavformat
      // reports samples per fragment.  No stco table exists to cross-check.
      spec.index_entries_present = false;
      spec.index_incomplete = false;
    } else {
      spec.index_entries_present = false;
      spec.index_incomplete = true;
    }
    tracks_.push_back(spec);
    out.tracks.push_back(spec);
  }
}

void Scanner::validate_structure(ScanOutcome& out) {
  Error base;
  base.file = path_;
  base.stage = Stage::Structure;
  base.severity = Severity::Recovered;

  // Missing moov sample tables: rebuildable, reported.
  for (const auto& spec : out.tracks) {
    if (!spec.index_entries_present && !out.fragmented) {
      Error e = base;
      e.track = static_cast<int>(spec.track_id);
      e.stream_index = spec.stream_index;
      e.code = Code::IndexMissing;
      e.byte_offset = out.structure.moov_offset >= 0 ? out.structure.moov_offset : 0;
      e.last_confirmed = out.structure.last_confirmed_offset;
      e.detail = "track " + std::to_string(spec.track_id) +
                 " has no usable sample table (stco/stsz)";
      e.action = "rebuild the index from packet boundaries";
      record(out, e);
      out.index_needs_rebuild = true;
    }
    if (spec.index_incomplete) {
      // The damaged bytes are the chunk-offset entries inside moov (where the
      // index lives), not the media the entries point at.  Report that entry's
      // own position; it is an exact 4- or 8-byte field, so the uncertainty is
      // the field width, not a packet.
      const TrackStructure* ct = container_track_for(out.structure, spec.track_id);
      Error e = base;
      e.track = static_cast<int>(spec.track_id);
      e.stream_index = spec.stream_index;
      e.code = Code::IndexEntryOutOfRange;
      const int64_t width = ct && ct->co64 ? 8 : 4;
      int64_t entry_off = ct ? ct->first_bad_entry_offset : -1;
      if (entry_off < 0) {
        entry_off = spec.first_bad_offset >= 0 ? spec.first_bad_offset
                                               : out.structure.file_size;
      }
      e.set_range(entry_off, entry_off + width);
      e.last_confirmed = out.structure.last_confirmed_offset;
      e.detail = std::to_string(spec.samples_out_of_range) +
                 " chunk offset entries address bytes past EOF; first bad sample " +
                 std::to_string(spec.first_bad_sample) + " (chunk points at " +
                 std::to_string(ct ? ct->first_bad_target : -1) + ", file is " +
                 std::to_string(out.file_size) + " bytes)";
      e.action = "rebuild the index by re-reading the present sample data";
      record(out, e);
      out.index_needs_rebuild = true;
      out.index_incomplete = true;
    }
  }

  // Samples whose declared span crosses EOF but start inside the file: the
  // tail was cut, not the index.  Reported at EOF, which is the first byte
  // that should exist and does not.
  for (const auto& spec : out.tracks) {
    if (spec.samples_past_eof <= 0) continue;
    Error e = base;
    e.track = static_cast<int>(spec.track_id);
    e.stream_index = spec.stream_index;
    e.code = Code::MdatTruncated;
    e.byte_offset = out.structure.file_size;
    e.last_confirmed = out.structure.file_size;
    e.detail = std::to_string(spec.samples_past_eof) + " samples of track " +
               std::to_string(spec.track_id) + " extend past EOF (first at sample " +
               std::to_string(spec.first_past_eof_sample) + ")";
    e.action = "drop the incomplete tail packet; every earlier packet is intact";
    record(out, e);
    out.truncated_tail = true;
  }

  // A truncated top-level box (typically mdat) means the tail is missing.
  if (out.structure.truncated) {
    Error e = base;
    e.code = out.structure.truncated_box == "moov" ? Code::MoovTruncated : Code::MdatTruncated;
    e.byte_offset = out.structure.first_truncated_offset >= 0
                        ? out.structure.first_truncated_offset
                        : out.file_size;
    e.last_confirmed = out.structure.last_confirmed_offset;
    e.detail = "top-level box '" + out.structure.truncated_box +
               "' extends past EOF; every byte before " +
               std::to_string(out.file_size) + " is present";
    e.action = "stream the complete packets up to EOF and stop at the short tail";
    record(out, e);
    out.truncated_tail = true;
  }

  // moov after mdat: the index is at the back, so a rebuild/relocation is
  // needed for random access.  Reported but not fatal.
  if (out.structure.moov_after_mdat) {
    Error e = base;
    e.code = Code::IndexRebuiltFromSizes;
    e.byte_offset = out.structure.moov_offset;
    e.last_confirmed = out.structure.last_confirmed_offset;
    e.detail = "moov follows mdat at byte " + std::to_string(out.structure.moov_offset) +
               "; the output is written with the index in front";
    e.action = "rewrite output with a leading moov";
    record(out, e);
    out.index_needs_rebuild = true;
  }

  // Unsupported streams cannot be verified; fail rather than silently drop.
  for (const auto& spec : out.tracks) {
    if (!spec.decodable) {
      Error e = base;
      e.track = static_cast<int>(spec.track_id);
      e.stream_index = spec.stream_index;
      e.severity = Severity::Fatal;
      e.code = Code::UnsupportedCodec;
      e.byte_offset = out.structure.moov_offset >= 0 ? out.structure.moov_offset : 0;
      e.detail = "no decoder for " + spec.codec_name + " (track " +
                 std::to_string(spec.track_id) + ")";
      e.action = "mark asset failed; output could not be verified";
      record(out, e);
      out.fatal = true;
      out.fatal_error = e;
    }
  }
}

bool Scanner::stream(const PacketCallback& cb, ScanOutcome& out) {
  if (!ctx_) return false;
  AVPacket* pkt = av_packet_alloc();
  if (!pkt) return false;
  bool ok = true;
  std::string why;

  // Map demuxer stream index -> container track, for per-packet accounting.
  auto track_for = [&](int index) -> const TrackSpec* {
    for (const auto& t : out.tracks) {
      if (t.stream_index == index) return &t;
    }
    return nullptr;
  };

  while (true) {
    const int ret = av_read_frame(ctx_, pkt);
    if (ret == AVERROR_EOF) break;
    if (ret == AVERROR(EIO) || ret < 0) {
      Error e;
      e.file = path_;
      e.stage = Stage::Scan;
      e.severity = Severity::Recovered;
      e.code = Code::PacketTruncated;
      e.byte_offset = out.structure.file_size;
      e.last_confirmed = out.structure.file_size;
      e.detail = "demuxer stopped early: " + av_error_string(ret);
      e.action = "finish at the last complete packet";
      record(out, e);
      break;
    }
    const int si = pkt->stream_index;
    const TrackSpec* spec = track_for(si);
    if (!spec) {
      av_packet_unref(pkt);
      continue;
    }
    const int64_t pos = pkt->pos >= 0 ? pkt->pos : -1;
    const int64_t size = pkt->size;
    if (size > limits_.max_packet_bytes) {
      Error e;
      e.file = path_;
      e.stage = Stage::Scan;
      e.severity = Severity::Fatal;
      e.code = Code::PacketTooLarge;
      e.track = static_cast<int>(spec->track_id);
      e.stream_index = si;
      e.set_range(pos >= 0 ? pos : 0, pos >= 0 ? pos + size : 0);
      e.detail = "packet of " + std::to_string(size) + " bytes exceeds the " +
                 std::to_string(limits_.max_packet_bytes) + " byte limit";
      e.action = "reject the asset; the declared length is out of bounds";
      record(out, e);
      out.fatal = true;
      out.fatal_error = e;
      av_packet_unref(pkt);
      ok = false;
      break;
    }
    if (pkt->flags & AV_PKT_FLAG_CORRUPT) {
      // The demuxer had to shorten this packet to fit the file (or found it
      // inconsistent).  It is not complete media; report the first missing
      // byte and stop, exactly as for a declared span crossing EOF.
      Error e;
      e.file = path_;
      e.stage = Stage::Scan;
      e.severity = Severity::Recovered;
      e.code = Code::PacketTruncated;
      e.track = static_cast<int>(spec->track_id);
      e.stream_index = si;
      const int64_t tail_at = out.file_size;
      e.set_range(pos >= 0 ? std::min(pos, tail_at) : tail_at, tail_at);
      e.last_confirmed = out.file_size;
      e.dts = pkt->dts;
      e.pts = pkt->pts;
      e.time_base = spec->time_base.str();
      e.detail = "demuxer marked the packet corrupt at byte " +
                 std::to_string(pos) + "; the file ends at " +
                 std::to_string(out.file_size);
      e.action = "drop the incomplete packet and stop at the last good byte";
      record(out, e);
      out.dropped_packets++;
      av_packet_unref(pkt);
      break;
    }
    if (pos < 0) {
      // A packet without a file position cannot be mapped back to a byte
      // range; report the offset as unknown rather than guessing.
      Error e;
      e.file = path_;
      e.stage = Stage::Scan;
      e.severity = Severity::Recovered;
      e.code = Code::OffsetUnknown;
      e.track = static_cast<int>(spec->track_id);
      e.stream_index = si;
      e.offset_known = false;
      e.detail = "packet has no container position; byte offset is unknown";
      e.action = "keep the packet but report an unknown offset";
      record(out, e);
    } else if (pos + size > out.file_size) {
      // The container promises more bytes than the file holds.  This is the
      // short tail: report the first byte the sample declares but the file
      // cannot contain, never the whole (short) packet FFmpeg handed us.
      Error e;
      e.file = path_;
      e.stage = Stage::Scan;
      e.severity = Severity::Recovered;
      e.code = Code::PacketTruncated;
      e.track = static_cast<int>(spec->track_id);
      e.stream_index = si;
      e.set_range(pos, out.file_size);
      e.last_confirmed = out.file_size;
      e.dts = pkt->dts;
      e.pts = pkt->pts;
      e.time_base = spec->time_base.str();
      e.detail = "packet at byte " + std::to_string(pos) + " declares " +
                 std::to_string(size) + " bytes but the file ends at " +
                 std::to_string(out.file_size) + "; first missing byte " +
                 std::to_string(out.file_size);
      e.action = "drop the incomplete packet; keep every complete packet before it";
      record(out, e);
      out.dropped_packets++;
      av_packet_unref(pkt);
      break;
    }
    if (size > 0) out.bytes_covered += size;
    ++out.packets;
    if (!cb(si, *pkt, why)) {
      ok = false;
      av_packet_unref(pkt);
      break;
    }
    av_packet_unref(pkt);
  }
  av_packet_free(&pkt);
  if (!ok && !why.empty()) out.stopped_by_sink = true;
  return ok;
}

}  // namespace mediaqc
