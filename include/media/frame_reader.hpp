#pragma once

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "media/delivery.hpp"
#include "media/types.hpp"

struct AVFormatContext;
struct AVPacket;

namespace mediaqc {

// Streams one track's media container through libavformat, pairing each
// demuxed packet with the next metadata record.  Packets are read in capture
// order; a media stream that ends early is reported as a truncated tail at the
// byte offset where reading stopped, not as a vague open failure.
class FrameReader {
 public:
  struct Options {
    // Hard cap on a single packet; guards against a corrupt container that
    // claims an absurd sample size.
    std::uint64_t max_packet_bytes = 64ull * 1024 * 1024;
    // When true, only packets are read (no decode).  The pipeline always
    // verifies payload checksums, so decoding is optional.
    bool decode = false;
  };

  struct Packet {
    std::int64_t pts = 0;
    bool has_pts = false;
    std::int64_t dts = 0;
    bool keyframe = false;
    std::uint64_t offset = 0;  // absolute byte offset of the packet payload
    std::uint64_t size = 0;
    std::vector<std::uint8_t> data;
  };

  FrameReader();
  ~FrameReader();
  FrameReader(const FrameReader&) = delete;
  FrameReader& operator=(const FrameReader&) = delete;

  // Opens `path`.  On failure `error` describes why (I/O vs. invalid media).
  bool open(const std::string& path, const TrackDesc& track, Options options,
            Error& error);

  // Reads the next packet.  Returns true on success, false at clean EOF or on
  // error.  `error` is set only when `reached_eof` would be false.
  bool next(Packet& packet, bool& reached_eof, Error& error);

  // Closes the container and returns the number of packets read.
  [[nodiscard]] std::uint64_t packets_read() const { return packets_read_; }
  [[nodiscard]] const std::string& path() const { return path_; }

  // Media duration in the stream's own time base, if the container knows it.
  [[nodiscard]] bool has_duration() const { return has_duration_; }
  [[nodiscard]] std::int64_t duration_ticks() const { return duration_ticks_; }
  [[nodiscard]] Rational time_base() const { return time_base_; }

  // Converts a stream timestamp to nanoseconds using the track clock.  The
  // container time base wins when it is present so timing survives re-muxes.
  [[nodiscard]] Ns pts_to_ns(std::int64_t pts) const;

  // Maps libav pixel formats onto our stable ids.
  [[nodiscard]] static PixelFormatId pixel_format_id(int av_pixel_format);

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
  std::string path_;
  std::uint64_t packets_read_ = 0;
  bool has_duration_ = false;
  std::int64_t duration_ticks_ = 0;
  Rational time_base_{1, 1};
  std::uint64_t max_packet_bytes_ = 64ull * 1024 * 1024;
  std::uint64_t file_size_ = 0;
};

}  // namespace mediaqc
