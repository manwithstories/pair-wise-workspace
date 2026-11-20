#include "media/index_writer.hpp"

#include "media/sha256.hpp"

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/channel_layout.h>
}

#include <algorithm>
#include <filesystem>
#include <string>

namespace mediaqc {
namespace {

namespace fs = std::filesystem;

Error make_error(ErrorCategory category, std::string code, std::string message) {
  Error error;
  error.category = category;
  error.code = std::move(code);
  error.message = std::move(message);
  return error;
}

std::string ffmpeg_error(int err) {
  char buffer[AV_ERROR_MAX_STRING_SIZE] = {0};
  av_strerror(err, buffer, sizeof buffer);
  return buffer;
}

std::string segment_name(const std::string& track, std::uint64_t index) {
  char suffix[16];
  std::snprintf(suffix, sizeof suffix, "%05llu",
                static_cast<unsigned long long>(index));
  return track + ".seg" + suffix + ".mp4";
}

}  // namespace

struct SegmentWriter::TrackState {
  AVFormatContext* format = nullptr;
  AVStream* stream = nullptr;
  AVCodecContext* encoder = nullptr;  // provides codec parameters only
  std::uint64_t segment_index = 0;
  std::uint64_t segment_first_frame = 0;
  std::uint64_t segment_frames = 0;
  Ns segment_start_ns = 0;
  std::string current_path;
  // Byte offsets of each packet in the current segment, resolved on close.
  struct PendingEntry {
    std::uint64_t frame = 0;
    std::uint64_t frame_index = 0;
    Ns time_ns = 0;
    bool keyframe = false;
    std::array<std::uint8_t, 32> sha256{};
  };
  std::vector<PendingEntry> pending;
  struct Resolved {
    std::uint64_t frame = 0;
    std::uint64_t frame_index = 0;
    Ns time_ns = 0;
    bool keyframe = false;
    std::array<std::uint8_t, 32> sha256{};
    std::size_t segment = 0;
    std::uint64_t offset = 0;
    std::uint64_t size = 0;
  };
  std::vector<Resolved> resolved;
  std::uint64_t next_frame_index = 0;
  std::uint64_t next_segment_index = 0;
};

SegmentWriter::SegmentWriter() = default;
SegmentWriter::~SegmentWriter() {
  if (opened_) abort();  // only unwind an unfinished run
}

void SegmentWriter::abort() {
  if (!opened_) {
    return;
  }
  for (auto& state : states_) {
    if (!state) continue;
    if (state->format) {
      if (state->format->pb) avio_closep(&state->format->pb);
      avformat_free_context(state->format);
      state->format = nullptr;
    }
    if (state->encoder) avcodec_free_context(&state->encoder);
    if (!state->current_path.empty()) {
      std::error_code ec;
      fs::remove(state->current_path, ec);
      state->current_path.clear();
    }
  }
  // Remove finalized segments we wrote during this run.
  for (const SegmentInfo& segment : segments_) {
    std::error_code ec;
    fs::remove(fs::path(output_dir_) / segment.path, ec);
  }
  segments_.clear();
  opened_ = false;
}

bool SegmentWriter::open(const std::string& output_dir,
                         const std::vector<TrackSpec>& tracks, bool overwrite,
                         Error& error, bool allow_existing) {
  output_dir_ = output_dir;
  tracks_ = tracks;
  std::error_code ec;
  fs::create_directories(fs::path(output_dir) / "media", ec);
  if (ec) {
    error = make_error(ErrorCategory::kIo, "output_dir_failed",
                       "cannot create " + output_dir + ": " + ec.message());
    return false;
  }
  const fs::path media_dir = fs::path(output_dir) / "media";
  bool has_segments = false;
  for (const auto& entry : fs::directory_iterator(media_dir, ec)) {
    if (ec) break;
    if (entry.path().extension() == ".mp4") {
      has_segments = true;
      break;
    }
  }
  if (overwrite) {
    for (const auto& entry : fs::directory_iterator(media_dir, ec)) {
      if (ec) break;
      if (entry.path().extension() == ".mp4") {
        std::error_code remove_ec;
        fs::remove(entry.path(), remove_ec);
      }
    }
  } else if (has_segments && !allow_existing) {
    error = make_error(ErrorCategory::kInvalidArgument, "output_exists",
                       "output already contains segments: " + output_dir);
    return false;
  }

  states_.clear();
  for (const TrackSpec& track : tracks) {
    auto state = std::make_unique<TrackState>();
    const AVCodec* encoder_codec =
        avcodec_find_encoder(track.desc.kind == TrackKind::Video
                                 ? AV_CODEC_ID_FFV1
                                 : AV_CODEC_ID_PCM_S16LE);
    if (!encoder_codec) {
      error = make_error(ErrorCategory::kInvalidInput, "unsupported_codec",
                         "no encoder for track " + track.desc.id);
      return false;
    }
    state->encoder = avcodec_alloc_context3(encoder_codec);
    AVCodecContext* context = state->encoder;
    context->time_base = AVRational{1, static_cast<int>(kNsPerSecond)};
    context->thread_count = 1;
    if (track.desc.kind == TrackKind::Video) {
      context->width = track.desc.width;
      context->height = track.desc.height;
      context->pix_fmt = AV_PIX_FMT_YUV420P;
      context->framerate = AVRational{track.desc.rate.num, track.desc.rate.den};
      context->gop_size = 10;
    } else {
      context->sample_fmt = AV_SAMPLE_FMT_S16;
      context->sample_rate = track.desc.sample_rate;
      av_channel_layout_default(&context->ch_layout, track.desc.channels);
    }
    if (avcodec_open2(context, encoder_codec, nullptr) < 0) {
      error = make_error(ErrorCategory::kIo, "encoder_open_failed",
                         "cannot open encoder for " + track.desc.id);
      return false;
    }
    states_.push_back(std::move(state));
  }
  opened_ = true;
  frames_written_ = 0;
  return true;
}

bool SegmentWriter::open_segment(std::size_t track_index, Error& error) {
  TrackState& state = *states_[track_index];
  const TrackSpec& spec = tracks_[track_index];
  const std::string name =
      segment_name(spec.desc.id, state.segment_index);
  const fs::path path = fs::path(output_dir_) / "media" / name;
  std::string path_str = path.string();

  AVFormatContext* format = nullptr;
  int ret = avformat_alloc_output_context2(&format, nullptr, "mp4", path_str.c_str());
  if (ret < 0 || !format) {
    error = make_error(ErrorCategory::kIo, "segment_alloc_failed",
                       "cannot create segment writer: " + ffmpeg_error(ret));
    return false;
  }
  AVStream* stream = avformat_new_stream(format, nullptr);
  if (!stream || avcodec_parameters_from_context(stream->codecpar, state.encoder) < 0) {
    error = make_error(ErrorCategory::kIo, "segment_stream_failed",
                       "cannot create stream for " + spec.desc.id);
    avformat_free_context(format);
    return false;
  }
  stream->time_base = AVRational{1, static_cast<int>(kNsPerSecond)};
  // Reopening the encoder per segment would change extradata; instead we reuse
  // the parameters from the single encoder opened in open().
  ret = avio_open(&format->pb, path_str.c_str(), AVIO_FLAG_WRITE);
  if (ret < 0) {
    error = make_error(ErrorCategory::kIo, "segment_open_failed",
                       "cannot open " + path_str + ": " + ffmpeg_error(ret));
    avformat_free_context(format);
    return false;
  }
  ret = avformat_write_header(format, nullptr);
  if (ret < 0) {
    error = make_error(ErrorCategory::kIo, "segment_header_failed",
                       "cannot write header for " + path_str + ": " +
                           ffmpeg_error(ret));
    avio_closep(&format->pb);
    avformat_free_context(format);
    return false;
  }
  state.format = format;
  state.stream = stream;
  state.current_path = path_str;
  state.segment_frames = 0;
  state.segment_start_ns = 0;
  state.pending.clear();
  return true;
}

bool SegmentWriter::close_segment(std::size_t track_index, Error& error) {
  TrackState& state = *states_[track_index];
  if (!state.format) return true;
  int ret = av_write_trailer(state.format);
  std::string path = state.current_path;
  if (ret < 0) {
    error = make_error(ErrorCategory::kIo, "segment_trailer_failed",
                       "cannot finalize " + path + ": " + ffmpeg_error(ret));
    return false;
  }
  if (state.format->pb) avio_closep(&state.format->pb);
  avformat_free_context(state.format);
  state.format = nullptr;
  state.stream = nullptr;

  // Resolve, for every pending frame, the byte offset of the output packet
  // that contains it, and digest the segment.
  AVFormatContext* probe = nullptr;
  ret = avformat_open_input(&probe, path.c_str(), nullptr, nullptr);
  if (ret < 0) {
    error = make_error(ErrorCategory::kIo, "segment_reopen_failed",
                       "cannot reopen " + path + ": " + ffmpeg_error(ret));
    return false;
  }
  ret = avformat_find_stream_info(probe, nullptr);
  if (ret < 0) {
    avformat_close_input(&probe);
    error = make_error(ErrorCategory::kIo, "segment_probe_failed",
                       "cannot probe " + path + ": " + ffmpeg_error(ret));
    return false;
  }

  struct OutPacket {
    Ns time_ns = 0;
    std::uint64_t offset = 0;
    std::uint64_t size = 0;
  };
  std::vector<OutPacket> packets;
  AVPacket* packet = av_packet_alloc();
  while (av_read_frame(probe, packet) >= 0) {
    if (packet->stream_index == 0) {
      OutPacket out;
      out.offset = packet->pos >= 0 ? static_cast<std::uint64_t>(packet->pos) : 0;
      out.size = static_cast<std::uint64_t>(packet->size);
      AVRational tb = probe->streams[0]->time_base;
      out.time_ns = packet->pts != AV_NOPTS_VALUE
                        ? av_rescale_q(packet->pts, tb, AVRational{1, 1000000000})
                        : 0;
      packets.push_back(out);
    }
    av_packet_unref(packet);
  }
  av_packet_free(&packet);
  avformat_close_input(&probe);

  SegmentInfo info;
  info.track = tracks_[track_index].desc.id;
  info.path = (fs::path("media") / fs::path(path).filename()).string();
  info.first_frame = state.segment_first_frame;
  info.frames = state.segment_frames;
  info.start_ns = state.pending.empty() ? 0 : state.pending.front().time_ns;
  info.end_ns = state.pending.empty() ? 0 : state.pending.back().time_ns;
  std::string sha_error;
  info.sha256 = to_hex(sha256_of_file(path, sha_error));
  std::error_code ec;
  info.bytes = fs::file_size(path, ec);
  const std::size_t segment_slot = segments_.size();
  segments_.push_back(info);
  state.current_path.clear();
  ++state.segment_index;

  // Map each source frame to the output packet that carries its timestamp.
  std::size_t cursor = 0;
  for (std::size_t i = 0; i < state.pending.size(); ++i) {
    const auto& pending = state.pending[i];
    while (cursor + 1 < packets.size() && packets[cursor + 1].time_ns <= pending.time_ns) {
      ++cursor;
    }
    if (packets.empty()) break;
    state.resolved.push_back({pending.frame, pending.frame_index, pending.time_ns,
                              pending.keyframe, pending.sha256, segment_slot,
                              packets[cursor].offset, packets[cursor].size});
  }
  state.pending.clear();
  return true;
}

bool SegmentWriter::write(std::size_t track_index, const FrameInput& frame,
                          IndexEntry& entry, Error& error) {
  if (!opened_ || track_index >= states_.size()) {
    error = make_error(ErrorCategory::kInvalidArgument, "bad_track",
                       "segment writer used with an unknown track");
    return false;
  }
  TrackState& state = *states_[track_index];
  if (!state.format) {
    state.segment_first_frame = frame.frame_index;
    if (!open_segment(track_index, error)) return false;
  }
  AVPacket* packet = av_packet_alloc();
  if (!packet || av_new_packet(packet, static_cast<int>(frame.payload.size())) < 0) {
    av_packet_free(&packet);
    error = make_error(ErrorCategory::kIo, "packet_alloc_failed",
                       "cannot allocate packet payload");
    return false;
  }
  if (!frame.payload.empty()) {
    std::memcpy(packet->data, frame.payload.data(), frame.payload.size());
  }
  packet->stream_index = 0;
  packet->pts = frame.time_ns;
  packet->dts = frame.time_ns;
  packet->duration = 1;
  if (frame.keyframe) packet->flags |= AV_PKT_FLAG_KEY;
  packet->pos = -1;
  int ret = av_interleaved_write_frame(state.format, packet);
  av_packet_free(&packet);
  if (ret < 0) {
    error = make_error(ErrorCategory::kIo, "segment_write_failed",
                       "cannot write frame: " + ffmpeg_error(ret));
    return false;
  }
  state.pending.push_back({frame.frame, frame.frame_index, frame.time_ns,
                           frame.keyframe, frame.sha256});
  ++state.segment_frames;
  ++frames_written_;

  // The entry is provisional; offsets are attached on segment close.  Callers
  // read the resolved entries from the index after finish().
  entry = IndexEntry{};
  entry.time_ns = frame.time_ns;
  entry.frame = frame.frame;
  entry.frame_index = frame.frame_index;
  entry.track = tracks_[track_index].desc.id;
  entry.sha256 = frame.sha256;
  entry.keyframe = frame.keyframe;

  if (state.segment_frames >= segment_frames_) {
    if (!close_segment(track_index, error)) return false;
  }
  return true;
}

bool SegmentWriter::resume(const ResumeState& state, Error& error) {
  if (!opened_) {
    error = make_error(ErrorCategory::kIo, "not_open",
                       "segment writer resume before open");
    return false;
  }
  if (state.next_segment_index.size() != states_.size() ||
      state.next_frame_index.size() != states_.size()) {
    error = make_error(ErrorCategory::kRecovery, "checkpoint_shape",
                       "checkpoint track count does not match the delivery");
    return false;
  }
  committed_segments_ = state.committed_segments;
  committed_entries_ = state.committed_entries;
  segment_slot_offset_ = state.max_segment_slot;
  for (std::size_t t = 0; t < states_.size(); ++t) {
    states_[t]->next_segment_index = state.next_segment_index[t];
    states_[t]->next_frame_index = state.next_frame_index[t];
  }
  // Start new segments after the committed ones.
  for (std::size_t t = 0; t < states_.size(); ++t) {
    states_[t]->segment_index = states_[t]->next_segment_index;
  }
  return true;
}

bool SegmentWriter::finish(MediaIndex& index, Error& error) {
  for (std::size_t t = 0; t < states_.size(); ++t) {
    if (states_[t]->format) {
      if (!close_segment(t, error)) return false;
    }
  }
  opened_ = false;

  index.tracks.clear();
  for (std::size_t t = 0; t < tracks_.size(); ++t) {
    const TrackDesc& desc = tracks_[t].desc;
    IndexTrack track;
    track.id = desc.id;
    track.kind = desc.kind;
    track.codec = desc.codec;
    track.width = desc.width;
    track.height = desc.height;
    track.sample_rate = desc.sample_rate;
    track.channels = desc.channels;
    track.rate = desc.rate;
    track.segments = states_[t]->segment_index;
    index.tracks.push_back(std::move(track));
  }
  index.segments = committed_segments_;
  index.segments.insert(index.segments.end(), segments_.begin(), segments_.end());

  // Emit entries grouped by track so find_by_* can binary-search a contiguous
  // block per track.  Within a track they are already time ordered.
  index.entries.clear();
  // Committed entries from earlier segments first, then this run's entries.
  // Both are grouped by track, so append per track in order.
  for (const IndexEntry& entry : committed_entries_) {
    index.entries.push_back(entry);
  }
  for (std::size_t t = 0; t < states_.size(); ++t) {
    auto& resolved = states_[t]->resolved;
    std::sort(resolved.begin(), resolved.end(),
              [](const auto& a, const auto& b) { return a.time_ns < b.time_ns; });
    for (const auto& r : resolved) {
      IndexEntry entry;
      entry.time_ns = r.time_ns;
      entry.frame = r.frame;
      entry.frame_index = r.frame_index;
      entry.track = tracks_[t].desc.id;
      entry.segment = r.segment + segment_slot_offset_;
      entry.payload_offset = r.offset;
      entry.payload_size = r.size;
      entry.keyframe = r.keyframe;
      entry.sha256 = r.sha256;
      index.entries.push_back(entry);
      index.tracks[t].entries++;
    }
    index.tracks[t].frames += resolved.size();
  }
  // Re-sort each track block by time so find_by_* sees ordered entries.
  std::stable_sort(index.entries.begin(), index.entries.end(),
                   [](const IndexEntry& a, const IndexEntry& b) {
                     if (a.track != b.track) return a.track < b.track;
                     return a.time_ns < b.time_ns;
                   });
  index.frames_total = index.entries.size();
  return true;
}

}  // namespace mediaqc
