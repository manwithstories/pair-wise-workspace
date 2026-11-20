#include "media/generate.hpp"

#include "media/delivery.hpp"
#include "media/fixture_generator.hpp"
#include "media/json.hpp"
#include "media/sha256.hpp"
#include "media/timeline.hpp"

#include <algorithm>
#include <filesystem>
#include <fstream>
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

struct TrackPlan {
  std::string id;
  TrackKind kind;
  std::string codec;
  int width = 0;
  int height = 0;
  int sample_rate = 0;
  int channels = 0;
  Rational rate;
  ClockSpec clock;
  VideoSpec video;
  AudioSpec audio;
  std::uint64_t frame_count = 0;
  std::vector<std::vector<std::uint8_t>> payloads;
  std::vector<std::int64_t> capture_ticks;  // per frame, raw device counter
  std::vector<bool> has_capture;
  std::vector<bool> keyframe;
  std::vector<std::uint64_t> seqs;
  std::vector<std::int64_t> damaged_lines;  // 1-based JSONL lines to corrupt
};

// One deterministic damage profile per scenario.  `damaged_lines` entries are
// 1-based line numbers; the generator flips one payload byte on that frame so
// the stored checksum no longer matches.
void apply_scenario(Scenario scenario, std::uint64_t frames,
                    std::vector<TrackPlan>& tracks) {
  if (tracks.empty()) return;
  TrackPlan& video = tracks[0];
  const std::uint64_t n = video.frame_count;
  if (n == 0) return;

  switch (scenario) {
    case Scenario::kNormal:
    case Scenario::kSlowConsumer:
      break;
    case Scenario::kWraparound:
      video.clock = ClockSpec{90000, 32};
      video.capture_ticks.assign(n, 0);
      video.has_capture.assign(n, true);
      {
        const std::uint64_t modulus = 1ULL << 32;
        const std::int64_t per_frame = 3600;
        const std::uint64_t start = modulus - static_cast<std::uint64_t>(per_frame) * 5;
        for (std::uint64_t i = 0; i < n; ++i) {
          video.capture_ticks[static_cast<std::size_t>(i)] =
              static_cast<std::int64_t>((start + i * static_cast<std::uint64_t>(per_frame)) % modulus);
        }
      }
      break;
    case Scenario::kDuplicate:
      if (n > 7) {
        video.damaged_lines = {std::int64_t{7}};
        video.seqs[6] = video.seqs[5];
        video.capture_ticks[6] = video.capture_ticks[5];
      }
      break;
    case Scenario::kBadChecksum:
      if (n > 11) {
        video.damaged_lines = {std::int64_t{11}};
      }
      break;
    case Scenario::kOutOfOrder:
      for (std::uint64_t i = 9; i < std::min<std::uint64_t>(n, 13); ++i) {
        std::swap(video.seqs[static_cast<std::size_t>(i)],
                  video.seqs[static_cast<std::size_t>(i + 1)]);
        std::swap(video.capture_ticks[static_cast<std::size_t>(i)],
                  video.capture_ticks[static_cast<std::size_t>(i + 1)]);
      }
      break;
    case Scenario::kTruncatedTail:
      // Handled by chopping the media container after metadata is written.
      break;
    case Scenario::kBoundary:
      // A little of everything, at fixed, well-separated offsets.
      video.clock = ClockSpec{90000, 32};
      if (n > 20) {
        video.has_capture[15] = false;
        video.capture_ticks[15] = 0;
      }
      if (n > 30) {
        video.capture_ticks[29] = video.capture_ticks[28];  // duplicate clock
      }
      if (n > 40) {
        std::swap(video.capture_ticks[39], video.capture_ticks[40]);  // reorder
      }
      if (n > 50) {
        video.damaged_lines = {51};
      }
      break;
  }
  (void)frames;
}

bool write_file(const fs::path& path, const std::string& content,
                std::string& error) {
  std::ofstream out(path, std::ios::binary | std::ios::trunc);
  if (!out) {
    error = "cannot write " + path.string();
    return false;
  }
  out.write(content.data(), static_cast<std::streamsize>(content.size()));
  if (!out) {
    error = "write failed for " + path.string();
    return false;
  }
  return true;
}

Json track_json(const TrackPlan& plan) {
  Json track = Json::make_object();
  track.set("id", Json::make_string(plan.id));
  track.set("kind", Json::make_string(std::string(to_string(plan.kind))));
  track.set("codec", Json::make_string(plan.codec));
  Json rate = Json::make_object();
  rate.set("num", Json::make_int(plan.rate.num));
  rate.set("den", Json::make_int(plan.rate.den));
  track.set("rate", std::move(rate));
  if (plan.kind == TrackKind::Video) {
    track.set("pix_fmt", Json::make_string("yuv420p"));
    track.set("width", Json::make_int(plan.width));
    track.set("height", Json::make_int(plan.height));
  } else {
    track.set("sample_rate", Json::make_int(plan.sample_rate));
    track.set("channels", Json::make_int(plan.channels));
  }
  Json clock = Json::make_object();
  if (plan.clock.bits == 0 && plan.clock.hz == static_cast<std::uint32_t>(kNsPerSecond)) {
    clock.set("kind", Json::make_string("ns"));
  } else {
    clock.set("kind", Json::make_string("ticks"));
    clock.set("hz", Json::make_int(plan.clock.hz));
    if (plan.clock.bits != 0) {
      clock.set("bits", Json::make_int(plan.clock.bits));
    }
  }
  track.set("clock", std::move(clock));
  track.set("depends_on", Json::make_array());
  track.set("required_fields", Json::make_array());
  track.set("optional_fields", Json::make_array());
  track.set("metadata_file", Json::make_string(plan.id + ".frames.jsonl"));
  track.set("media_file", Json::make_string(plan.id + ".mkv"));
  return track;
}

}  // namespace

bool parse_scenario(std::string_view text, Scenario& out) {
  if (text == "normal") { out = Scenario::kNormal; return true; }
  if (text == "boundary") { out = Scenario::kBoundary; return true; }
  if (text == "wraparound") { out = Scenario::kWraparound; return true; }
  if (text == "duplicate") { out = Scenario::kDuplicate; return true; }
  if (text == "bad-checksum" || text == "bad_checksum") {
    out = Scenario::kBadChecksum;
    return true;
  }
  if (text == "out-of-order" || text == "out_of_order") {
    out = Scenario::kOutOfOrder;
    return true;
  }
  if (text == "truncated-tail" || text == "truncated") {
    out = Scenario::kTruncatedTail;
    return true;
  }
  if (text == "slow-consumer" || text == "slow_consumer") {
    out = Scenario::kSlowConsumer;
    return true;
  }
  return false;
}

std::string_view to_string(Scenario scenario) {
  switch (scenario) {
    case Scenario::kNormal: return "normal";
    case Scenario::kBoundary: return "boundary";
    case Scenario::kWraparound: return "wraparound";
    case Scenario::kDuplicate: return "duplicate";
    case Scenario::kBadChecksum: return "bad-checksum";
    case Scenario::kOutOfOrder: return "out-of-order";
    case Scenario::kTruncatedTail: return "truncated-tail";
    case Scenario::kSlowConsumer: return "slow-consumer";
  }
  return "normal";
}

std::vector<std::string> scenario_names() {
  return {"normal",     "boundary",  "wraparound",     "duplicate",
          "bad-checksum", "out-of-order", "truncated-tail", "slow-consumer"};
}

GenerateResult generate_delivery(const std::string& out_dir,
                                 const GenerateOptions& options,
                                 bool allow_existing) {
  GenerateResult result;
  if (options.frames == 0) {
    result.error = make_error(ErrorCategory::kInvalidArgument, "bad_frames",
                              "--frames must be positive");
    return result;
  }

  fs::path root(out_dir);
  std::error_code ec;
  if (fs::exists(root, ec)) {
    if (!fs::is_directory(root, ec)) {
      result.error = make_error(ErrorCategory::kInvalidArgument, "not_a_directory",
                                "output path " + out_dir + " is not a directory");
      return result;
    }
    bool empty = fs::is_empty(root, ec);
    if (!empty && !allow_existing) {
      result.error = make_error(ErrorCategory::kInvalidArgument,
                                "output_exists",
                                "output directory " + out_dir +
                                    " is not empty (refusing to overwrite)");
      return result;
    }
  } else if (!fs::create_directories(root, ec) || ec) {
    result.error = make_error(ErrorCategory::kIo, "mkdir_failed",
                              "cannot create " + out_dir + ": " + ec.message());
    return result;
  }

  // Video carries two thirds of the frames, audio the rest.  The split is
  // deterministic so the frame count is reproducible.
  const std::uint64_t video_frames = options.frames * 2 / 3;
  const std::uint64_t audio_frames = options.frames - video_frames;

  std::vector<TrackPlan> tracks(2);
  TrackPlan& video = tracks[0];
  video.id = "video_main";
  video.kind = TrackKind::Video;
  video.codec = "ffv1";
  video.width = 64;
  video.height = 48;
  video.rate = Rational{25, 1};
  video.clock = ClockSpec{static_cast<std::uint32_t>(kNsPerSecond), 0};
  video.video = VideoSpec{video.width, video.height, video.rate, video_frames,
                          options.seed};
  video.frame_count = video_frames;

  TrackPlan& audio = tracks[1];
  audio.id = "audio_main";
  audio.kind = TrackKind::Audio;
  audio.codec = "pcm_s16le";
  audio.sample_rate = 48000;
  audio.channels = 1;
  audio.rate = Rational{50, 1};
  audio.clock = ClockSpec{static_cast<std::uint32_t>(kNsPerSecond), 0};
  audio.audio = AudioSpec{audio.sample_rate, audio.channels, audio_frames, 960,
                          options.seed};
  audio.frame_count = audio_frames;

  for (TrackPlan& plan : tracks) {
    plan.payloads.reserve(static_cast<std::size_t>(plan.frame_count));
    plan.capture_ticks.resize(static_cast<std::size_t>(plan.frame_count));
    plan.has_capture.assign(static_cast<std::size_t>(plan.frame_count), true);
    plan.keyframe.assign(static_cast<std::size_t>(plan.frame_count), false);
    plan.seqs.resize(static_cast<std::size_t>(plan.frame_count));
    const Ns interval = TrackTimeline::compute_interval_ns(plan.rate);
    for (std::uint64_t i = 0; i < plan.frame_count; ++i) {
      plan.capture_ticks[static_cast<std::size_t>(i)] =
          static_cast<std::int64_t>(i) * interval;
      plan.seqs[static_cast<std::size_t>(i)] = i;
      plan.keyframe[static_cast<std::size_t>(i)] = (i % 10) == 0;
    }
  }

  apply_scenario(options.scenario, options.frames, tracks);

  // Write media containers, capturing the exact encoded packets so checksums
  // cover the bytes that will actually be read back.
  for (TrackPlan& plan : tracks) {
    std::string error;
    // Only the damaged lines get their payload byte flipped after the media is
    // written; the container itself is always well-formed except for the
    // explicit truncated-tail scenario.
    if (plan.kind == TrackKind::Video) {
      if (!write_video_container((root / (plan.id + ".mkv")).string(), plan.video,
                                 &plan.payloads, error)) {
        result.error = make_error(ErrorCategory::kIo, "media_write_failed", error);
        return result;
      }
    } else {
      if (!write_audio_container((root / (plan.id + ".mkv")).string(), plan.audio,
                                 &plan.payloads, error)) {
        result.error = make_error(ErrorCategory::kIo, "media_write_failed", error);
        return result;
      }
    }
    if (plan.payloads.size() != plan.frame_count) {
      result.error = make_error(
          ErrorCategory::kIo, "payload_count_mismatch",
          "encoder produced " + std::to_string(plan.payloads.size()) +
              " packets, expected " + std::to_string(plan.frame_count));
      return result;
    }
  }

  // Write metadata streams.  Corrupted scenarios flip one byte of the stored
  // payload before hashing, so the recorded checksum disagrees with the media.
  std::uint64_t total_frames = 0;
  for (std::size_t t = 0; t < tracks.size(); ++t) {
    TrackPlan& plan = tracks[t];
    std::string content;
    content.reserve(static_cast<std::size_t>(plan.frame_count) * 160);
    for (std::uint64_t i = 0; i < plan.frame_count; ++i) {
      std::vector<std::uint8_t> payload = plan.payloads[static_cast<std::size_t>(i)];
      const bool damage =
          std::find(plan.damaged_lines.begin(), plan.damaged_lines.end(),
                    static_cast<std::int64_t>(i + 1)) != plan.damaged_lines.end();
      if (damage && !payload.empty()) {
        payload[payload.size() / 2] ^= 0xFF;
      }
      Sha256Digest digest = sha256(payload.data(), payload.size());
      Json record = Json::make_object();
      record.set("seq", Json::make_int(static_cast<std::int64_t>(
                            plan.seqs[static_cast<std::size_t>(i)])));
      if (plan.has_capture[static_cast<std::size_t>(i)]) {
        record.set("capture_ticks", Json::make_int(
                                        plan.capture_ticks[static_cast<std::size_t>(i)]));
      } else {
        record.set("capture_ticks", Json::make_null());
      }
      record.set("keyframe", Json::make_bool(plan.keyframe[static_cast<std::size_t>(i)]));
      if (plan.kind == TrackKind::Video) {
        record.set("pix_fmt", Json::make_string("yuv420p"));
        record.set("width", Json::make_int(plan.width));
        record.set("height", Json::make_int(plan.height));
      }
      record.set("sha256", Json::make_string(to_hex(digest)));
      Json shooting = Json::make_object();
      shooting.set("camera_id", Json::make_string(plan.id == "video_main" ? "cam-a" : "cam-a"));
      shooting.set("scene", Json::make_int(static_cast<std::int64_t>(i / 100)));
      record.set("shooting", std::move(shooting));
      if (damage) {
        result.first_corrupt_offset = static_cast<std::int64_t>(content.size());
      }
      content += dump_json(record);
      content.push_back('\n');
      ++total_frames;
    }
    std::string error;
    if (!write_file(root / (plan.id + ".frames.jsonl"), content, error)) {
      result.error = make_error(ErrorCategory::kIo, "write_failed", error);
      return result;
    }
  }

  // Truncated-tail scenario: chop the last container after metadata is written
  // so the tail of the media no longer matches its records.
  if (options.scenario == Scenario::kTruncatedTail) {
    fs::path media = root / "video_main.mkv";
    std::uintmax_t size = fs::file_size(media, ec);
    if (!ec && size > 1024) {
      fs::resize_file(media, size * 3 / 4, ec);
      if (ec) {
        result.error = make_error(ErrorCategory::kIo, "truncate_failed",
                                  "cannot truncate " + media.string() + ": " +
                                      ec.message());
        return result;
      }
    }
  }

  Json delivery = Json::make_object();
  delivery.set("schema_version", Json::make_int(kSchemaVersion));
  delivery.set("producer", Json::make_string("mediaqc-sample-generator"));
  delivery.set("created", Json::make_string("1970-01-01T00:00:00Z"));
  delivery.set("scenario", Json::make_string(std::string(to_string(options.scenario))));
  delivery.set("seed", Json::make_int(static_cast<std::int64_t>(options.seed)));
  Json track_array = Json::make_array();
  for (const TrackPlan& plan : tracks) {
    track_array.push(track_json(plan));
  }
  delivery.set("tracks", std::move(track_array));

  std::string error;
  if (!write_file(root / "delivery.json", dump_json(delivery, true) + "\n", error)) {
    result.error = make_error(ErrorCategory::kIo, "write_failed", error);
    return result;
  }

  result.ok = true;
  result.frames = total_frames;
  result.tracks = tracks.size();
  return result;
}

}  // namespace mediaqc
