#include "relay/stage_cache.hpp"

#include <sstream>

namespace relay {
namespace {

void write_track_spec(std::ostream& out, const TrackSpec& spec) {
  out << spec.kind << ' ' << (spec.required ? 1 : 0) << ' '
      << (spec.policy.loss_mode == LossMode::kBurst ? "burst" : "single") << ' '
      << spec.depends_on.size();
  for (const auto& dependency : spec.depends_on) {
    out << ' ' << dependency;
  }
  out << '\n';
}

bool read_track_spec(std::istream& in, TrackSpec& spec, std::string& error) {
  std::string loss_mode;
  std::size_t dependencies = 0;
  int required = 1;
  if (!(in >> spec.kind >> required >> loss_mode >> dependencies)) {
    error = "truncated track spec";
    return false;
  }
  spec.required = required != 0;
  spec.policy.loss_mode =
      loss_mode == "single" ? LossMode::kSingleFrame : LossMode::kBurst;
  for (std::size_t i = 0; i < dependencies; ++i) {
    std::string dependency;
    if (!(in >> dependency)) {
      error = "truncated dependency list";
      return false;
    }
    spec.depends_on.push_back(dependency);
  }
  return true;
}

}  // namespace

std::string serialize_read_cache(const ReadResult& result) {
  std::ostringstream out;
  out << "RELAYREAD 1\n";
  out << "digest " << result.input_digest << '\n';
  out << "failures " << result.checksum_failures << ' ' << result.decode_failures
      << '\n';
  out << "tracks " << result.tracks.size() << '\n';
  for (const auto& track : result.tracks) {
    out << "TRACK " << track.spec.id << ' ' << track.timebase.fps_num << ' '
        << track.timebase.fps_den << ' ' << track.reorder_window << ' '
        << track.checksum_failures << ' ' << track.decode_failures << '\n';
    write_track_spec(out, track.spec);
    out << "DIGEST " << track.file_digest << '\n';
    out << "FRAMES " << track.catalog.size() << '\n';
    for (const auto& catalog_frame : track.catalog) {
      const FrameRecord& frame = catalog_frame.record;
      out << "FRAME " << frame.source_frame << ' ' << frame.sequence << ' '
          << frame.raw_timestamp_us << ' ' << (frame.keyframe ? 1 : 0) << ' '
          << frame.pixel_format << ' ' << frame.width << ' ' << frame.height
          << ' ' << frame.exposure.iso << ' ' << frame.exposure.shutter_us
          << ' ' << frame.exposure.aperture << ' ' << frame.sha256 << ' '
          << frame.payload_bytes << ' ' << (catalog_frame.verified ? 1 : 0)
          << '\n';
    }
    out << "ARRIVALS " << track.arrivals.size() << '\n';
    for (const auto& arrival : track.arrivals) {
      out << arrival.arrival_index << ' ' << arrival.source_frame << '\n';
    }
  }
  return out.str();
}

bool parse_read_cache(const std::string& text, ReadResult& out,
                      std::string& error) {
  std::istringstream in(text);
  std::string magic;
  std::getline(in, magic);
  if (magic != "RELAYREAD 1") {
    error = "read cache magic rejected";
    return false;
  }
  std::string key;
  ReadResult parsed;
  std::size_t track_count = 0;
  if (!(in >> key >> parsed.input_digest) || key != "digest") {
    error = "read cache digest missing";
    return false;
  }
  if (!(in >> key >> parsed.checksum_failures >> parsed.decode_failures) ||
      key != "failures") {
    error = "read cache failure counters missing";
    return false;
  }
  if (!(in >> key >> track_count) || key != "tracks") {
    error = "read cache track count missing";
    return false;
  }
  for (std::size_t t = 0; t < track_count; ++t) {
    TrackInput track;
    if (!(in >> key >> track.spec.id >> track.timebase.fps_num >>
          track.timebase.fps_den >> track.reorder_window >>
          track.checksum_failures >> track.decode_failures) ||
        key != "TRACK") {
      error = "read cache track header truncated";
      return false;
    }
    if (!read_track_spec(in, track.spec, error)) {
      return false;
    }
    if (!(in >> key >> track.file_digest) || key != "DIGEST") {
      error = "read cache track digest missing";
      return false;
    }
    std::size_t frame_count = 0;
    if (!(in >> key >> frame_count) || key != "FRAMES") {
      error = "read cache frame count missing";
      return false;
    }
    for (std::size_t f = 0; f < frame_count; ++f) {
      CatalogFrame catalog_frame;
      FrameRecord& frame = catalog_frame.record;
      frame.track = track.spec.id;
      int keyframe = 0;
      int verified = 0;
      if (!(in >> key >> frame.source_frame >> frame.sequence >>
            frame.raw_timestamp_us >> keyframe >> frame.pixel_format >>
            frame.width >> frame.height >> frame.exposure.iso >>
            frame.exposure.shutter_us >> frame.exposure.aperture >>
            frame.sha256 >> frame.payload_bytes >> verified) ||
          key != "FRAME") {
        error = "read cache frame record truncated";
        return false;
      }
      frame.keyframe = keyframe != 0;
      catalog_frame.verified = verified != 0;
      track.catalog.push_back(std::move(catalog_frame));
    }
    std::size_t arrival_count = 0;
    if (!(in >> key >> arrival_count) || key != "ARRIVALS") {
      error = "read cache arrival count missing";
      return false;
    }
    for (std::size_t a = 0; a < arrival_count; ++a) {
      Arrival arrival;
      if (!(in >> arrival.arrival_index >> arrival.source_frame)) {
        error = "read cache arrival truncated";
        return false;
      }
      track.arrivals.push_back(arrival);
    }
    parsed.tracks.push_back(std::move(track));
  }
  out = std::move(parsed);
  return true;
}

std::string serialize_reorder_cache(const std::vector<TrackStream>& streams) {
  std::ostringstream out;
  out << "RELAYREORDER 1\n";
  out << "streams " << streams.size() << '\n';
  for (const auto& stream : streams) {
    out << "STREAM " << stream.track << ' ' << stream.timebase.fps_num << ' '
        << stream.timebase.fps_den << ' ' << stream.epochs.size() << '\n';
    out << "STATS " << stream.stats.received << ' ' << stream.stats.duplicates
        << ' ' << stream.stats.reordered << ' ' << stream.stats.late_dropped
        << ' ' << stream.stats.lost_marked << ' ' << stream.stats.epochs << ' '
        << stream.stats.unverified_quarantined << ' '
        << stream.stats.peak_cache_bytes << '\n';
    for (const auto& epoch : stream.epochs) {
      out << "EPOCH " << epoch.index << ' ' << epoch.origin_us << ' '
          << epoch.first_sequence << ' ' << epoch.duplicates << ' '
          << epoch.late_dropped << ' ' << epoch.frames.size() << ' '
          << epoch.losses.size() << '\n';
      out << "CAUSE " << epoch.cause << '\n';
      for (const auto& frame : epoch.frames) {
        out << "FRAME " << frame.source_frame << ' ' << frame.sequence << ' '
            << frame.raw_timestamp_us << ' ' << frame.pts_us << ' '
            << frame.epoch << ' ' << (frame.keyframe ? 1 : 0) << ' '
            << frame.pixel_format << ' ' << frame.width << ' ' << frame.height
            << ' ' << frame.sha256 << ' ' << frame.payload_bytes << '\n';
      }
      for (const auto& loss : epoch.losses) {
        out << "LOSS " << loss.first_sequence << ' ' << loss.count << ' '
            << loss.epoch << '\n';
      }
    }
  }
  return out.str();
}

bool parse_reorder_cache(const std::string& text,
                         std::vector<TrackStream>& out, std::string& error) {
  std::istringstream in(text);
  std::string magic;
  std::getline(in, magic);
  if (magic != "RELAYREORDER 1") {
    error = "reorder cache magic rejected";
    return false;
  }
  std::string key;
  std::size_t stream_count = 0;
  if (!(in >> key >> stream_count) || key != "streams") {
    error = "reorder cache stream count missing";
    return false;
  }
  std::vector<TrackStream> parsed;
  for (std::size_t s = 0; s < stream_count; ++s) {
    TrackStream stream;
    std::size_t epoch_count = 0;
    if (!(in >> key >> stream.track >> stream.timebase.fps_num >>
          stream.timebase.fps_den >> epoch_count) ||
        key != "STREAM") {
      error = "reorder cache stream header truncated";
      return false;
    }
    if (!(in >> key >> stream.stats.received >> stream.stats.duplicates >>
          stream.stats.reordered >> stream.stats.late_dropped >>
          stream.stats.lost_marked >> stream.stats.epochs >>
          stream.stats.unverified_quarantined >>
          stream.stats.peak_cache_bytes) ||
        key != "STATS") {
      error = "reorder cache stats truncated";
      return false;
    }
    for (std::size_t e = 0; e < epoch_count; ++e) {
      TimelineEpoch epoch;
      std::size_t frame_count = 0;
      std::size_t loss_count = 0;
      if (!(in >> key >> epoch.index >> epoch.origin_us >>
            epoch.first_sequence >> epoch.duplicates >> epoch.late_dropped >>
            frame_count >> loss_count) ||
          key != "EPOCH") {
        error = "reorder cache epoch header truncated";
        return false;
      }
      if (!(in >> key) || key != "CAUSE") {
        error = "reorder cache epoch cause missing";
        return false;
      }
      std::getline(in, epoch.cause);
      if (!epoch.cause.empty() && epoch.cause.front() == ' ') {
        epoch.cause.erase(epoch.cause.begin());
      }
      for (std::size_t f = 0; f < frame_count; ++f) {
        FrameRecord frame;
        frame.track = stream.track;
        int keyframe = 0;
        if (!(in >> key >> frame.source_frame >> frame.sequence >>
              frame.raw_timestamp_us >> frame.pts_us >> frame.epoch >>
              keyframe >> frame.pixel_format >> frame.width >>
              frame.height >> frame.sha256 >> frame.payload_bytes) ||
            key != "FRAME") {
          error = "reorder cache frame truncated";
          return false;
        }
        frame.keyframe = keyframe != 0;
        epoch.frames.push_back(std::move(frame));
      }
      for (std::size_t l = 0; l < loss_count; ++l) {
        LostRange loss;
        loss.track = stream.track;
        if (!(in >> key >> loss.first_sequence >> loss.count >> loss.epoch) ||
            key != "LOSS") {
          error = "reorder cache loss truncated";
          return false;
        }
        epoch.losses.push_back(std::move(loss));
      }
      stream.epochs.push_back(std::move(epoch));
    }
    parsed.push_back(std::move(stream));
  }
  out = std::move(parsed);
  return true;
}

}  // namespace relay
