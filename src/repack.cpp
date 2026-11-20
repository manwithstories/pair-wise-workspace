#include "media/repack.hpp"

#include <algorithm>
#include <cstring>
#include <fstream>
#include <unistd.h>

extern "C" {
#include <libavutil/channel_layout.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
}

#include "media/fs_util.hpp"

namespace mediaqc {
namespace {

void put_u16(std::string& s, uint16_t v) {
    s.push_back(static_cast<char>(v & 0xFF));
    s.push_back(static_cast<char>((v >> 8) & 0xFF));
}
void put_u32(std::string& s, uint32_t v) {
    for (int i = 0; i < 4; ++i) s.push_back(static_cast<char>((v >> (8 * i)) & 0xFF));
}

std::string wav_header(int channels, int rate, int bps, int64_t data_bytes) {
    std::string h;
    h.reserve(44);
    h.append("RIFF", 4);
    put_u32(h, static_cast<uint32_t>(36 + data_bytes));
    h.append("WAVEfmt ", 8);
    put_u32(h, 16);
    put_u16(h, 1);                                      // PCM
    put_u16(h, static_cast<uint16_t>(channels));
    put_u32(h, static_cast<uint32_t>(rate));
    const int block = channels * bps / 8;
    put_u32(h, static_cast<uint32_t>(rate * block));    // byte rate
    put_u16(h, static_cast<uint16_t>(block));
    put_u16(h, static_cast<uint16_t>(bps));
    h.append("data", 4);
    put_u32(h, static_cast<uint32_t>(data_bytes));
    return h;
}

// Damaged source frame intervals (in source sample coordinates).
std::vector<Segment> damaged_source_frames(const ScanResult& s) {
    std::vector<Segment> out;
    for (const auto& d : s.damage)
        if (d.end_sample > d.start_sample) out.push_back({d.start_sample, d.end_sample});
    std::sort(out.begin(), out.end(),
              [](const Segment& a, const Segment& b) { return a.start_sample < b.start_sample; });
    std::vector<Segment> merged;
    for (const auto& seg : out) {
        if (!merged.empty() && seg.start_sample <= merged.back().end_sample)
            merged.back().end_sample = std::max(merged.back().end_sample, seg.end_sample);
        else
            merged.push_back(seg);
    }
    return merged;
}

bool is_damaged(const std::vector<Segment>& ranges, int64_t frame) {
    auto it = std::upper_bound(ranges.begin(), ranges.end(), frame,
                               [](int64_t v, const Segment& s) { return v < s.start_sample; });
    if (it == ranges.begin()) return false;
    --it;
    return frame < it->end_sample;
}

}  // namespace

RepackResult repack(const ProbeResult& probe, const ScanResult& scan,
                    const RepackConfig& cfg, const std::string& validated_out_dir,
                    const std::string& output_name) {
    const auto& h = probe.header;
    if (h.block_align <= 0) throw std::runtime_error("cannot repack: invalid source header");
    const int bps = h.bits_per_sample;
    const int64_t src_frame_b = h.block_align;
    const int64_t available = std::max<int64_t>(0, h.available_data_size);
    const int64_t usable_frames = (available / src_frame_b);
    const auto damaged = damaged_source_frames(scan);

    const bool same_layout = h.sample_rate == cfg.target_rate_hz &&
                             h.channels == cfg.target_channels;
    const int64_t src_bitrate = static_cast<int64_t>(h.sample_rate) * h.channels * bps;
    const int64_t target = cfg.target_bitrate > 0 ? cfg.target_bitrate
                            : static_cast<int64_t>(cfg.target_rate_hz) * cfg.target_channels * bps;
    const bool within_tolerance =
        cfg.passthrough_tolerance_bps <= 0
            ? src_bitrate == target
            : std::llabs(src_bitrate - target) <= cfg.passthrough_tolerance_bps;
    const bool passthrough = same_layout && within_tolerance;

    RepackResult r;
    r.output_rate_hz = passthrough ? h.sample_rate : cfg.target_rate_hz;
    r.output_channels = passthrough ? h.channels : cfg.target_channels;
    r.bits_per_sample = bps;
    r.pcm_reused = passthrough;

    const std::string final_path = fs::safe_join(validated_out_dir, output_name);
    const std::string tmp_path = final_path + ".part." +
        std::to_string(static_cast<long long>(::getpid()));

    std::ifstream in(probe.path, std::ios::binary);
    if (!in) throw std::runtime_error("cannot reopen source: " + probe.path);
    std::ofstream out(tmp_path, std::ios::binary | std::ios::trunc);
    if (!out) throw std::runtime_error("cannot open output: " + tmp_path);
    out.write(wav_header(r.output_channels, r.output_rate_hz, bps, 0).data(), 44);

    auto copy_frame_range = [&](int64_t begin, int64_t end, bool mark_policy) {
        if (end <= begin) return;
        const int64_t off = h.data_offset + begin * src_frame_b;
        const int64_t bytes = (end - begin) * src_frame_b;
        in.seekg(static_cast<std::streamoff>(off));
        std::vector<char> buf(static_cast<size_t>(std::min<int64_t>(bytes, 1 << 20)));
        int64_t left = bytes;
        while (left > 0) {
            const size_t n = static_cast<size_t>(std::min<int64_t>(left, buf.size()));
            in.read(buf.data(), static_cast<std::streamsize>(n));
            if (in.gcount() <= 0) throw std::runtime_error("source read failed during repack");
            out.write(buf.data(), in.gcount());
            left -= in.gcount();
        }
        if (mark_policy) r.policy_ranges.push_back({begin, end});
    };

    if (passthrough) {
        // Passthrough: reuse PCM bytes directly, substituting silence or
        // cutting at damaged frame ranges.
        int64_t cursor = 0;
        for (const auto& seg : damaged) {
            const int64_t ds = std::max<int64_t>(0, seg.start_sample);
            const int64_t de = std::min(seg.end_sample, usable_frames);
            if (ds >= de) continue;
            copy_frame_range(cursor, ds, false);
            if (cfg.damage_policy == DamagePolicy::Silence) {
                const std::vector<char> zeros(static_cast<size_t>((de - ds) * src_frame_b), 0);
                out.write(zeros.data(), static_cast<std::streamsize>(zeros.size()));
                r.silence_frames += de - ds;
            } else {
                r.dropped_frames += de - ds;
            }
            r.policy_ranges.push_back({ds, de});
            cursor = de;
        }
        if (cfg.damage_policy == DamagePolicy::Silence)
            copy_frame_range(cursor, usable_frames, false);
        else if (cursor < usable_frames)
            copy_frame_range(cursor, usable_frames, false);
        r.output_frames = usable_frames - r.dropped_frames;
    } else {
        // Conversion path via libswresample (rate + channel layout).
        SwrContext* swr = nullptr;
        AVChannelLayout in_layout, out_layout;
        av_channel_layout_default(&in_layout, h.channels);
        av_channel_layout_default(&out_layout, r.output_channels);
        int rc = swr_alloc_set_opts2(
            &swr, &out_layout, AV_SAMPLE_FMT_S16, r.output_rate_hz,
            &in_layout, AV_SAMPLE_FMT_S16, h.sample_rate, 0, nullptr);
        if (rc < 0 || swr_init(swr) < 0) {
            if (swr) swr_free(&swr);
            av_channel_layout_uninit(&in_layout);
            av_channel_layout_uninit(&out_layout);
            throw std::runtime_error("swresample init failed");
        }
        const int out_frame_b = r.output_channels * bps / 8;
        AVFrame* fin = av_frame_alloc();
        AVFrame* fout = av_frame_alloc();
        const int chunk = 4096;
        const int cap = av_rescale_rnd(chunk, r.output_rate_hz, h.sample_rate, AV_ROUND_UP) + 256;
        fin->format = AV_SAMPLE_FMT_S16;
        fin->ch_layout = in_layout;
        fin->sample_rate = h.sample_rate;
        fin->nb_samples = chunk;
        av_frame_get_buffer(fin, 0);
        fout->format = AV_SAMPLE_FMT_S16;
        fout->ch_layout = out_layout;
        fout->sample_rate = r.output_rate_hz;
        fout->nb_samples = cap;
        av_frame_get_buffer(fout, 0);
        for (int64_t base = 0; base < usable_frames; base += chunk) {
            const int n = static_cast<int>(std::min<int64_t>(chunk, usable_frames - base));
            std::vector<char> raw(static_cast<size_t>(n) * src_frame_b);
            in.seekg(static_cast<std::streamoff>(h.data_offset + base * src_frame_b));
            in.read(raw.data(), static_cast<std::streamsize>(raw.size()));
            for (int i = 0; i < n; ++i)
                if (is_damaged(damaged, base + i))
                    std::memset(raw.data() + static_cast<size_t>(i) * src_frame_b, 0, src_frame_b);
            std::memcpy(fin->data[0], raw.data(), raw.size());
            const int got = swr_convert(swr, fout->data, cap, fin->data, n);
            if (got > 0) {
                out.write(reinterpret_cast<const char*>(fout->data[0]),
                          static_cast<std::streamsize>(got) * out_frame_b);
                r.output_frames += got;
            }
        }
        // Drain the resampler tail.
        const int tail = swr_convert(swr, fout->data, cap, nullptr, 0);
        if (tail > 0) {
            out.write(reinterpret_cast<const char*>(fout->data[0]),
                      static_cast<std::streamsize>(tail) * out_frame_b);
            r.output_frames += tail;
        }
        av_frame_free(&fout);
        av_frame_free(&fin);
        swr_free(&swr);
        av_channel_layout_uninit(&in_layout);
        av_channel_layout_uninit(&out_layout);
    }

    const int64_t out_frame_b = r.output_channels * bps / 8;
    r.output_bytes = r.output_frames * out_frame_b;
    r.actual_bitrate_bps = r.output_frames > 0
        ? static_cast<int64_t>(static_cast<long double>(r.output_bytes * 8) *
                               r.output_rate_hz / r.output_frames)
        : 0;
    r.bitrate_deviation_pct = target > 0
        ? 100.0 * static_cast<double>(r.actual_bitrate_bps - target) / static_cast<double>(target)
        : 0.0;
    out.close();

    // Patch the header with the real data size and atomically replace.
    std::string final_header = wav_header(r.output_channels, r.output_rate_hz, bps, r.output_bytes);
    {
        std::fstream p(tmp_path, std::ios::in | std::ios::out | std::ios::binary);
        p.write(final_header.data(), static_cast<std::streamsize>(final_header.size()));
    }
    std::error_code ec;
    std::filesystem::rename(tmp_path, final_path, ec);
    if (ec) {
        std::filesystem::remove(tmp_path);
        throw std::runtime_error("atomic replace failed: " + ec.message());
    }
    r.output_path = final_path;
    return r;
}

}  // namespace mediaqc
