#include "fixture.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>

namespace mb {

namespace {
constexpr u32 kVideoClock = 90000;
constexpr u32 kAudioClock = 48000;
constexpr u16 kAudioChannels = 2;
constexpr u32 kFramesPerAudioPacket = 960;  // 20 ms at 48 kHz
constexpr u32 kKeyframeInterval = 30;      // 1 s GOP at 30 fps

struct StreamSpec {
  u16 track_id;
  u32 ssrc;
  TrackKind kind;
  u32 clock_rate;
  u16 channels;
  const char* name;
  double drift_ppm;
};
constexpr StreamSpec kSpecs[] = {
    {0, 0xC0FFEE01, TrackKind::Video, kVideoClock, 0, "cam-a", 0.0},
    {1, 0xC0FFEE02, TrackKind::Video, kVideoClock, 0, "cam-b", 0.0},
    {2, 0xC0FFEE03, TrackKind::Video, kVideoClock, 0, "cam-c", 1500.0},
    {3, 0xC0FFEE10, TrackKind::Audio, kAudioClock, kAudioChannels,
     "aud-1", 0.0},
};
}  // namespace

std::vector<u8> FixtureGenerator::synth_video(DeterministicRng& rng,
                                              u32 frame, bool keyframe) {
  // Content is derived purely from the frame index, so cameras covering the
  // same scene produce byte-identical payloads (cam-c seeds a divergent view).
  (void)rng;
  std::vector<u8> p;
  p.reserve(96);
  wr32(p, frame);
  wr32(p, keyframe ? 0x49445231 : 0x50314652);  // "IDR1" / "P1FR"
  DeterministicRng content(u64(frame) * 11995408973635179863ULL +
                           0xA5A5A5A5A5A5A5A5ULL);
  for (int i = 0; i < 11; ++i) {
    u32 v = static_cast<u32>(content.next() >> 32);
    wr32(p, v ^ (frame * 2654435761u + (u32(i) * 97u)));
  }
  return p;
}

std::vector<u8> FixtureGenerator::synth_audio(DeterministicRng& rng,
                                              u32 packet, u16 channels,
                                              u32 sample_rate,
                                              bool& silence) {
  (void)sample_rate;
  // 200 ms silence around 3 s and 6 s; elsewhere a deterministic sine.
  double t0 = double(packet) * 20.0e-3;
  silence = (t0 >= 3.0 && t0 < 3.2) || (t0 >= 6.0 && t0 < 6.2);
  std::vector<u8> p;
  p.reserve(std::size_t(channels) * kFramesPerAudioPacket * 2u);
  for (u32 f = 0; f < kFramesPerAudioPacket; ++f) {
    for (u16 c = 0; c < channels; ++c) {
      std::int16_t s = 0;
      if (!silence) {
        double phase = 6.283185307179586 * 440.0 *
                       (double(packet * kFramesPerAudioPacket + f) /
                        double(kAudioClock));
        double tone = 0.25 * std::sin(phase + double(c) * 0.3);
        s = static_cast<std::int16_t>(tone * 32767.0);
      }
      (void)rng;  // tone is fully deterministic; rng reserved for noise model
      p.push_back(u8(u16(s) & 0xFFu));
      p.push_back(u8((u16(s) >> 8) & 0xFFu));
    }
  }
  return p;
}

GeneratedFixture FixtureGenerator::generate() {
  GeneratedFixture out;
  DeterministicRng rng(opt_.seed);
  for (const auto& s : kSpecs) {
    TrackInfo t;
    t.track_id = s.track_id;
    t.ssrc = s.ssrc;
    t.kind = s.kind;
    t.clock_rate = s.clock_rate;
    t.channels = s.channels;
    t.name = s.name;
    t.drift_ppm = s.drift_ppm;
    out.tracks.push_back(t);
  }

  u32 flags = (opt_.truncate_tail ? 1u : 0u) |
              (opt_.omit_index_pages ? 2u : 0u);
  CaptureWriter w(opt_.seed, flags, static_cast<u16>(out.tracks.size()));
  for (const auto& t : out.tracks) w.write_track_decl(t);

  auto build_stream = [&](const StreamSpec& spec) {
    DeterministicRng local(opt_.seed * 1000u + u64(spec.track_id) + 1u);
    u32 count = spec.kind == TrackKind::Video ? opt_.video_frames
                                              : opt_.audio_packets;
    std::vector<PacketRecord> stream;
    u32 seq = static_cast<u32>(local.next() % 60000u);
    u32 rtp = static_cast<u32>(local.next());
    for (u32 i = 0; i < count; ++i) {
      bool key = spec.kind == TrackKind::Video && (i % kKeyframeInterval == 0);
      bool sil = false;
      std::vector<u8> payload =
          spec.kind == TrackKind::Video
              ? synth_video(local, i, key)
              : synth_audio(local, i, spec.channels, spec.clock_rate, sil);
      // cam-c records a different angle: perturb payload bytes so hashes
      // diverge while the fingerprint stays in the candidate band.
      if (spec.track_id == 2) {
        for (std::size_t b = 4; b < payload.size(); b += 5)
          payload[b] = u8(payload[b] ^ 0x5Au);
      }
      PacketRecord p;
      p.ssrc = spec.ssrc;
      p.seq = seq++;
      p.abs_seq = p.seq;
      p.rtp_ts = rtp;
      p.marker = spec.kind == TrackKind::Audio ? sil : key;
      p.keyframe = key;
      p.payload = std::move(payload);
      if (local.uniform() < opt_.corrupt_probability) {
        std::fill(p.payload.begin(), p.payload.end(), u8(0xFF));
      }
      stream.push_back(std::move(p));
      rtp += spec.kind == TrackKind::Video
                 ? (kVideoClock / 30u)
                 : kFramesPerAudioPacket;
    }
    // Loss first, then bounded reorder swaps (inside reorder window).
    std::vector<PacketRecord> kept;
    for (auto& p : stream) {
      if (local.uniform() < opt_.loss_probability) continue;
      kept.push_back(std::move(p));
    }
    for (std::size_t i = 1; i < kept.size(); ++i) {
      if (local.uniform() < opt_.reorder_probability) {
        std::size_t back = i - std::size_t(1 + local.next() % 3u);
        std::swap(kept[i], kept[back]);
      }
    }
    // RTCP SRs anchor the wall clock at fixed packet intervals.
    u64 wall_ns = 1'700'000'000ULL * u64(kNsPerSec);
    for (u32 i = 0; i < kept.size(); ++i) {
      if (i % opt_.sr_every == 0) {
        u32 ntp_sec = u32(wall_ns / u64(kNsPerSec)) + 2208988800u;
        u32 ntp_frac = u32((wall_ns % u64(kNsPerSec)) << 32 / u64(kNsPerSec));
        w.write_rtcp_sr(spec.track_id, spec.ssrc, ntp_sec, ntp_frac,
                        kept[i].rtp_ts);
      }
      w.write_rtp(spec.track_id, kept[i]);
      wall_ns += spec.kind == TrackKind::Video
                     ? (kNsPerSec / 30u)
                     : (kNsPerSec * kFramesPerAudioPacket / kAudioClock);
    }
    if (!opt_.omit_index_pages && spec.kind == TrackKind::Video) {
      std::vector<IndexEntry> page;
      for (const auto& p : kept) {
        if (p.keyframe) page.push_back({p.rtp_ts, 0, 1});
      }
      w.write_index_page(spec.track_id, page);
    }
    out.expected_packets += kept.size();
  };

  for (const auto& spec : kSpecs) build_stream(spec);
  w.write_eos();
  out.complete_capture = w.bytes();
  out.capture = out.complete_capture;
  if (opt_.truncate_tail) {
    // Cut inside the last ~3% of records so the tail is a partial record.
    std::size_t cut = out.capture.size() * 97u / 100u;
    out.capture.resize(cut);
  }
  out.expected_segments = 8u * 4u;  // ~1 s segments over four tracks
  return out;
}

}  // namespace mb
