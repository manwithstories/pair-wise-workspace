#include "media/index_writer.hpp"

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/channel_layout.h>
#include <libavutil/opt.h>
}

#include <algorithm>
#include <cstring>
#include <filesystem>
#include <string>
#include <vector>

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

enum class CodecChoice { kFfv1, kPcmS16, kUnsupported };

CodecChoice codec_for(const TrackDesc& desc) {
  if (desc.kind == TrackKind::Video) {
    return CodecChoice::kFfv1;
  }
  if (desc.kind == TrackKind::Audio) {
    return CodecChoice::kPcmS16;
  }
  return CodecChoice::kUnsupported;
}

}  // namespace

struct IndexWriter::Impl {
  AVFormatContext* format = nullptr;
  std::vector<AVStream*> streams;
  std::vector<AVCodecContext*> codecs;   // owned encoder contexts (extradata)
  std::vector<TrackSpec> specs;
  // Per-packet records, indexed by write order; offsets resolved at close.
  struct Record {
    std::size_t track = 0;
    Ns time_ns = 0;
    std::uint64_t frame = 0;
    bool keyframe = false;
    std::array<std::uint8_t, 32> sha256{};
  };
  // Source frames in write order, one per accepted metadata record.
  std::vector<Record> records;
  bool opened = false;
  bool failed = false;
};

IndexWriter::IndexWriter() : impl_(std::make_unique<Impl>()) {}

IndexWriter::~IndexWriter() { abort(); }

void IndexWriter::abort() {
  if (!impl_ || !impl_->opened) {
    return;
  }
  impl_->opened = false;
  if (impl_->format) {
    for (AVCodecContext* codec : impl_->codecs) {
      if (codec) avcodec_free_context(&codec);
    }
    if (impl_->format->pb) avio_closep(&impl_->format->pb);
    avformat_free_context(impl_->format);
    impl_->format = nullptr;
  }
  std::error_code ec;
  fs::remove(output_path_, ec);
}

bool IndexWriter::open(const std::string& output_path,
                       const std::vector<TrackSpec>& tracks, bool overwrite,
                       Error& error) {
  output_path_ = output_path;
  std::error_code ec;
  if (!overwrite && fs::exists(output_path, ec)) {
    error = make_error(ErrorCategory::kInvalidArgument, "output_exists",
                       "output " + output_path + " already exists");
    return false;
  }
  fs::path parent = fs::path(output_path).parent_path();
  if (!parent.empty() && !fs::exists(parent, ec)) {
    fs::create_directories(parent, ec);
    if (ec) {
      error = make_error(ErrorCategory::kIo, "mkdir_failed",
                         "cannot create " + parent.string() + ": " + ec.message());
      return false;
    }
  }

  int ret = avformat_alloc_output_context2(&impl_->format, nullptr, "mp4",
                                           output_path.c_str());
  if (ret < 0 || !impl_->format) {
    error = make_error(ErrorCategory::kIo, "output_alloc_failed",
                       "cannot create MP4 writer: " + ffmpeg_error(ret));
    return false;
  }

  impl_->specs = tracks;
  impl_->streams.resize(tracks.size(), nullptr);
  impl_->codecs.resize(tracks.size(), nullptr);

  for (std::size_t i = 0; i < tracks.size(); ++i) {
    const TrackDesc& desc = tracks[i].desc;
    CodecChoice choice = codec_for(desc);
    const AVCodec* codec = nullptr;
    AVCodecID id = AV_CODEC_ID_NONE;
    if (choice == CodecChoice::kFfv1) {
      codec = avcodec_find_encoder(AV_CODEC_ID_FFV1);
      id = AV_CODEC_ID_FFV1;
    } else if (choice == CodecChoice::kPcmS16) {
      codec = avcodec_find_encoder(AV_CODEC_ID_PCM_S16LE);
      id = AV_CODEC_ID_PCM_S16LE;
    }
    if (!codec) {
      error = make_error(ErrorCategory::kInvalidInput, "unsupported_codec",
                         "track " + desc.id + ": output codec unavailable");
      abort();
      return false;
    }

    AVCodecContext* context = avcodec_alloc_context3(codec);
    if (!context) {
      error = make_error(ErrorCategory::kIo, "encoder_alloc_failed",
                         "cannot allocate encoder for " + desc.id);
      abort();
      return false;
    }
    context->time_base = AVRational{1, static_cast<int>(kNsPerSecond)};
    context->thread_count = 1;
    if (choice == CodecChoice::kFfv1) {
      context->width = desc.width;
      context->height = desc.height;
      context->pix_fmt = AV_PIX_FMT_YUV420P;
      context->framerate = AVRational{desc.rate.num, desc.rate.den};
      context->gop_size = 10;
    } else {
      context->sample_fmt = AV_SAMPLE_FMT_S16;
      context->sample_rate = desc.sample_rate;
      av_channel_layout_default(&context->ch_layout, desc.channels);
    }
    // FFV1 needs an open encoder to produce the extradata the MP4 muxer wants.
    if (avcodec_open2(context, codec, nullptr) < 0) {
      error = make_error(ErrorCategory::kIo, "encoder_open_failed",
                         "cannot open encoder for " + desc.id);
      avcodec_free_context(&context);
      abort();
      return false;
    }

    AVStream* stream = avformat_new_stream(impl_->format, nullptr);
    if (!stream) {
      error = make_error(ErrorCategory::kIo, "stream_alloc_failed",
                         "cannot create stream for " + desc.id);
      avcodec_free_context(&context);
      abort();
      return false;
    }
    avcodec_parameters_from_context(stream->codecpar, context);
    stream->time_base = context->time_base;
    av_dict_set(&stream->metadata, "mediaqc_track", desc.id.c_str(), 0);
    av_dict_set(&stream->metadata, "mediaqc_kind",
                std::string(to_string(desc.kind)).c_str(), 0);
    impl_->streams[i] = stream;
    impl_->codecs[i] = context;
    (void)id;
  }

  ret = avio_open(&impl_->format->pb, output_path.c_str(), AVIO_FLAG_WRITE);
  if (ret < 0) {
    error = make_error(ErrorCategory::kIo, "output_open_failed",
                       "cannot open " + output_path + ": " + ffmpeg_error(ret));
    abort();
    return false;
  }
  ret = avformat_write_header(impl_->format, nullptr);
  if (ret < 0) {
    error = make_error(ErrorCategory::kIo, "header_failed",
                       "cannot write MP4 header: " + ffmpeg_error(ret));
    abort();
    return false;
  }
  impl_->opened = true;
  return true;
}

bool IndexWriter::write(const PacketInput& packet, Error& error) {
  if (!impl_->opened || impl_->failed) {
    error = make_error(ErrorCategory::kIo, "not_open",
                       "index writer is not open");
    return false;
  }
  if (packet.track_index >= impl_->streams.size()) {
    error = make_error(ErrorCategory::kInvalidArgument, "bad_track",
                       "packet references unknown track");
    return false;
  }
  AVStream* stream = impl_->streams[packet.track_index];
  AVPacket* avp = av_packet_alloc();
  if (!avp) {
    error = make_error(ErrorCategory::kIo, "packet_alloc_failed",
                       "cannot allocate packet");
    return false;
  }
  if (av_new_packet(avp, static_cast<int>(packet.payload.size())) < 0) {
    av_packet_free(&avp);
    error = make_error(ErrorCategory::kIo, "packet_alloc_failed",
                       "cannot allocate packet payload");
    return false;
  }
  if (!packet.payload.empty()) {
    std::memcpy(avp->data, packet.payload.data(), packet.payload.size());
  }
  avp->stream_index = stream->index;
  avp->pts = packet.time_ns;
  avp->dts = packet.time_ns;
  avp->duration = 1;
  if (packet.keyframe) {
    avp->flags |= AV_PKT_FLAG_KEY;
  }
  avp->pos = -1;

  // Record the interleaved packet; offsets are assigned after the trailer is
  // flushed because the sample table precedes the payload it describes.
  Impl::Record record;
  record.track = packet.track_index;
  record.time_ns = packet.time_ns;
  record.frame = packet.frame;
  record.keyframe = packet.keyframe;
  record.sha256 = packet.sha256;
  impl_->records.push_back(record);

  int ret = av_interleaved_write_frame(impl_->format, avp);
  av_packet_free(&avp);
  if (ret < 0) {
    error = make_error(ErrorCategory::kIo, "write_failed",
                       "cannot write packet: " + ffmpeg_error(ret));
    impl_->failed = true;
    return false;
  }
  ++packets_written_;
  return true;
}

bool IndexWriter::close(MediaIndex& index, Error& error) {
  if (!impl_->opened) {
    error = make_error(ErrorCategory::kIo, "not_open", "index writer is not open");
    return false;
  }
  if (impl_->failed) {
    abort();
    error = make_error(ErrorCategory::kIo, "write_failed",
                       "a prior packet write failed");
    return false;
  }
  int ret = av_write_trailer(impl_->format);
  if (ret < 0) {
    abort();
    error = make_error(ErrorCategory::kIo, "trailer_failed",
                       "cannot finalize MP4: " + ffmpeg_error(ret));
    return false;
  }
  // Sample offsets are available on each stream now that the trailer assigned
  // them; read the finished file back to translate packet ordinals into byte
  // ranges.  This keeps the index honest even if the muxer reordered packets.
  for (AVCodecContext* codec : impl_->codecs) {
    if (codec) avcodec_free_context(&codec);
  }
  if (impl_->format->pb) avio_closep(&impl_->format->pb);
  impl_->opened = false;

  // Reopen the finished file and collect per-packet offsets.  Streams are
  // matched by the mediaqc_track tag written above.
  AVFormatContext* probe = nullptr;
  ret = avformat_open_input(&probe, output_path_.c_str(), nullptr, nullptr);
  if (ret < 0) {
    error = make_error(ErrorCategory::kIo, "output_reopen_failed",
                       "cannot reopen " + output_path_ + ": " +
                           ffmpeg_error(ret));
    return false;
  }
  ret = avformat_find_stream_info(probe, nullptr);
  if (ret < 0) {
    avformat_close_input(&probe);
    error = make_error(ErrorCategory::kIo, "output_probe_failed",
                       "cannot probe output: " + ffmpeg_error(ret));
    return false;
  }

  MediaIndex built;
  built.tracks.resize(impl_->specs.size());
  for (std::size_t i = 0; i < impl_->specs.size(); ++i) {
    const TrackDesc& desc = impl_->specs[i].desc;
    IndexTrack& track = built.tracks[i];
    track.id = desc.id;
    track.kind = desc.kind;
    track.codec = desc.codec;
    track.width = desc.width;
    track.height = desc.height;
    track.sample_rate = desc.sample_rate;
    track.channels = desc.channels;
    track.rate = desc.rate;
  }

  // Collect the output packets per stream, in file order.  The muxer may
  // re-chunk some codecs (notably PCM), so packet count and boundaries do not
  // have to match the source frames one-to-one.
  struct OutputPacket {
    Ns time_ns = 0;
    std::uint64_t offset = 0;
    std::uint64_t size = 0;
    std::int64_t pts = 0;
    bool has_pts = false;
  };
  std::vector<std::vector<OutputPacket>> packets_by_stream(built.tracks.size());
  AVPacket* packet = av_packet_alloc();
  while (av_read_frame(probe, packet) >= 0) {
    const auto stream_index = static_cast<std::size_t>(packet->stream_index);
    if (stream_index < packets_by_stream.size()) {
      OutputPacket out;
      out.pts = packet->pts;
      out.has_pts = packet->pts != AV_NOPTS_VALUE;
      out.offset = packet->pos >= 0 ? static_cast<std::uint64_t>(packet->pos) : 0;
      out.size = static_cast<std::uint64_t>(packet->size);
      // Convert to nanoseconds using the demuxed stream's time base.
      AVRational tb = probe->streams[packet->stream_index]->time_base;
      if (out.has_pts) {
        out.time_ns = av_rescale_q(packet->pts, tb, AVRational{1, 1000000000});
      }
      packets_by_stream[stream_index].push_back(out);
    }
    av_packet_unref(packet);
  }
  av_packet_free(&packet);
  avformat_close_input(&probe);

  // For every accepted source frame, find the output packet on its stream that
  // contains its timestamp.  Video and audio both write monotonically, so a
  // single forward pass per stream suffices.
  std::vector<std::size_t> cursor(built.tracks.size(), 0);
  for (const Impl::Record& record : impl_->records) {
    if (record.track >= packets_by_stream.size()) continue;
    const auto& stream_packets = packets_by_stream[record.track];
    if (stream_packets.empty()) continue;
    std::size_t& at = cursor[record.track];
    while (at + 1 < stream_packets.size() &&
           stream_packets[at + 1].time_ns <= record.time_ns) {
      ++at;
    }
    const OutputPacket& out = stream_packets[at];
    IndexEntry entry;
    entry.time_ns = record.time_ns;
    entry.frame = record.frame;
    entry.track = built.tracks[record.track].id;
    entry.output_packet = at;
    entry.payload_offset = out.offset;
    // Constant-bitrate codecs (raw video, PCM) pack samples contiguously, so
    // the source frame's slice inside an output packet is proportional to its
    // position.  Use the whole packet size for one-to-one video codecs.
    entry.payload_size = out.size;
    entry.keyframe = record.keyframe;
    entry.sha256 = record.sha256;
    built.entries.push_back(std::move(entry));
    ++built.tracks[record.track].entries;
  }
  // Stable order for random access: by time, then by track id.
  std::stable_sort(built.entries.begin(), built.entries.end(),
                   [](const IndexEntry& a, const IndexEntry& b) {
                     if (a.time_ns != b.time_ns) return a.time_ns < b.time_ns;
                     return a.track < b.track;
                   });

  // Duration and size.
  std::error_code ec;
  std::uintmax_t file_size = fs::file_size(output_path_, ec);
  (void)file_size;
  Ns duration = 0;
  for (const IndexEntry& entry : built.entries) {
    duration = std::max(duration, entry.time_ns);
  }
  // Extend by one frame interval using the video rate when available.
  for (const auto& spec : impl_->specs) {
    if (spec.desc.kind == TrackKind::Video && spec.desc.rate.valid()) {
      duration += (kNsPerSecond * spec.desc.rate.den) / spec.desc.rate.num;
      break;
    }
  }
  built.duration_ns = duration;
  built.frames_total = built.entries.size();

  index = std::move(built);
  return true;
}

}  // namespace mediaqc
