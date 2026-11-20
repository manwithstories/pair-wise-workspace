#include "media/frame_reader.hpp"

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/pixfmt.h>
}

#include <cerrno>
#include <cstring>
#include <filesystem>
#include <string>

namespace mediaqc {
namespace {

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

}  // namespace

struct FrameReader::Impl {
  AVFormatContext* format = nullptr;
  AVPacket* packet = nullptr;
  std::size_t stream_index = 0;
  bool eof = false;
  bool opened = false;
};

FrameReader::FrameReader() : impl_(std::make_unique<Impl>()) {}

FrameReader::~FrameReader() {
  if (impl_) {
    if (impl_->packet) {
      av_packet_free(&impl_->packet);
    }
    if (impl_->format) {
      avformat_close_input(&impl_->format);
    }
  }
}

PixelFormatId FrameReader::pixel_format_id(int av_pixel_format) {
  switch (av_pixel_format) {
    case AV_PIX_FMT_YUV420P: return PixelFormatId::kYuv420p;
    case AV_PIX_FMT_YUV422P: return PixelFormatId::kYuv422p;
    case AV_PIX_FMT_YUV444P: return PixelFormatId::kYuv444p;
    case AV_PIX_FMT_GRAY8: return PixelFormatId::kGray;
    case AV_PIX_FMT_RGB24: return PixelFormatId::kRgb24;
    default: return PixelFormatId::kUnknown;
  }
}

bool FrameReader::open(const std::string& path, const TrackDesc& track,
                       Options options, Error& error) {
  path_ = path;
  max_packet_bytes_ = options.max_packet_bytes;

  std::error_code ec;
  file_size_ = std::filesystem::file_size(path, ec);
  if (ec) {
    error = make_error(ErrorCategory::kIo, "open_failed",
                       "cannot stat " + path + ": " + ec.message());
    error.track = track.id;
    return false;
  }

  int ret = avformat_open_input(&impl_->format, path.c_str(), nullptr, nullptr);
  if (ret < 0) {
    error = make_error(ErrorCategory::kCorruption, "open_failed",
                       "cannot open media " + path + ": " + ffmpeg_error(ret));
    error.track = track.id;
    return false;
  }
  ret = avformat_find_stream_info(impl_->format, nullptr);
  if (ret < 0) {
    error = make_error(ErrorCategory::kCorruption, "probe_failed",
                       "cannot probe media " + path + ": " + ffmpeg_error(ret));
    error.track = track.id;
    return false;
  }

  // Pick the first stream matching the declared kind.
  const AVMediaType want =
      track.kind == TrackKind::Audio ? AVMEDIA_TYPE_AUDIO : AVMEDIA_TYPE_VIDEO;
  impl_->stream_index = static_cast<std::size_t>(-1);
  for (unsigned i = 0; i < impl_->format->nb_streams; ++i) {
    if (impl_->format->streams[i]->codecpar->codec_type == want) {
      impl_->stream_index = i;
      break;
    }
  }
  if (impl_->stream_index == static_cast<std::size_t>(-1)) {
    error = make_error(ErrorCategory::kInvalidInput, "missing_stream",
                       "media " + path + " has no " +
                           std::string(track.kind == TrackKind::Audio ? "audio"
                                                                      : "video") +
                           " stream");
    error.track = track.id;
    return false;
  }

  AVStream* stream = impl_->format->streams[impl_->stream_index];
  time_base_ = Rational{stream->time_base.num, stream->time_base.den};
  if (stream->duration != AV_NOPTS_VALUE && stream->duration > 0) {
    has_duration_ = true;
    duration_ticks_ = stream->duration;
  }

  // The container's codec is authoritative; a mismatch with the declaration is
  // invalid input rather than silent acceptance.
  const char* codec_name = avcodec_get_name(stream->codecpar->codec_id);
  if (!track.codec.empty() && codec_name &&
      track.codec != std::string(codec_name)) {
    error = make_error(ErrorCategory::kInvalidInput, "codec_mismatch",
                       "media " + path + " codec '" + codec_name +
                           "' does not match declared '" + track.codec + "'");
    error.track = track.id;
    return false;
  }

  impl_->packet = av_packet_alloc();
  if (!impl_->packet) {
    error = make_error(ErrorCategory::kIo, "alloc_failed",
                       "cannot allocate packet for " + path);
    error.track = track.id;
    return false;
  }
  impl_->eof = false;
  impl_->opened = true;
  return true;
}

bool FrameReader::next(Packet& packet, bool& reached_eof, Error& error) {
  reached_eof = false;
  if (!impl_->opened) {
    error = make_error(ErrorCategory::kInvalidInput, "not_open",
                       "frame reader used before open");
    return false;
  }
  if (impl_->eof) {
    reached_eof = true;
    return false;
  }

  while (true) {
    int ret = av_read_frame(impl_->format, impl_->packet);
    if (ret == AVERROR_EOF) {
      impl_->eof = true;
      reached_eof = true;
      return false;
    }
    if (ret < 0) {
      // A read error at EOF is how a truncated file usually surfaces.  Point
      // the operator at the byte where demuxing stopped.
      error = make_error(ErrorCategory::kCorruption, "truncated_media",
                         "media " + path_ + " ended prematurely: " +
                             ffmpeg_error(ret));
      error.has_offset = true;
      error.offset = file_size_;
      impl_->eof = true;
      return false;
    }
    if (static_cast<std::size_t>(impl_->packet->stream_index) !=
        impl_->stream_index) {
      av_packet_unref(impl_->packet);
      continue;
    }

    if (static_cast<std::uint64_t>(impl_->packet->size) > max_packet_bytes_) {
      error = make_error(ErrorCategory::kCorruption, "packet_too_large",
                         "media " + path_ + " packet " + std::to_string(packets_read_) +
                             " is " + std::to_string(impl_->packet->size) +
                             " bytes, above the limit");
      error.has_offset = true;
      error.offset = impl_->packet->pos >= 0
                         ? static_cast<std::uint64_t>(impl_->packet->pos)
                         : file_size_;
      av_packet_unref(impl_->packet);
      return false;
    }

    packet.pts = impl_->packet->pts;
    packet.has_pts = impl_->packet->pts != AV_NOPTS_VALUE;
    packet.dts = impl_->packet->dts;
    packet.keyframe = (impl_->packet->flags & AV_PKT_FLAG_KEY) != 0;
    packet.offset = impl_->packet->pos >= 0
                        ? static_cast<std::uint64_t>(impl_->packet->pos)
                        : 0;
    packet.size = static_cast<std::uint64_t>(impl_->packet->size);
    packet.data.assign(impl_->packet->data,
                       impl_->packet->data + impl_->packet->size);
    av_packet_unref(impl_->packet);
    ++packets_read_;
    return true;
  }
}

Ns FrameReader::pts_to_ns(std::int64_t pts) const {
  if (!time_base_.valid()) {
    return pts;
  }
  // pts * 1e9 * num / den, rounded, with overflow-safe splitting.
  const std::int64_t num = static_cast<std::int64_t>(time_base_.num);
  const std::int64_t den = static_cast<std::int64_t>(time_base_.den);
  const __int128 scaled = static_cast<__int128>(pts) * num * kNsPerSecond;
  const __int128 denom = static_cast<__int128>(den);
  __int128 value = scaled / denom;
  const __int128 rem = scaled % denom;
  if (rem * 2 >= denom) {
    ++value;
  } else if (rem * 2 <= -denom) {
    --value;
  }
  return static_cast<Ns>(value);
}

}  // namespace mediaqc
