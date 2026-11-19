#include "silence.hpp"
#include <algorithm>
#include <cmath>
#include <cstring>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/channel_layout.h>
#include <libavutil/frame.h>
#include <libavutil/log.h>
#include <libswresample/swresample.h>
}

namespace md {
namespace {
constexpr uint32_t TARGET_RATE = 16000;
constexpr uint32_t RING_CAP_SECONDS = 1; // 有界环形缓存上限（秒）

// 单窗口 RMS，满幅千分比阈值；返回 RMS（0..1000）
uint32_t rms_milli(const int16_t* s, size_t n) {
    if (n == 0) return 0;
    uint64_t acc = 0;
    for (size_t i = 0; i < n; ++i) acc += uint64_t(s[i]) * uint64_t(s[i]);
    double rms = std::sqrt(double(acc) / double(n));
    return uint32_t(rms * 1000.0 / 32768.0);
}
} // namespace

struct SilenceDetector::Impl {
    const AVCodec* dec = nullptr;
    AVCodecContext* dctx = nullptr;
    SwrContext* swr = nullptr;
    AVFrame* frame = nullptr;
    AVPacket* pkt = nullptr;
    bool init_failed = false;

    explicit Impl(bool need_aac) {
        av_log_set_level(AV_LOG_QUIET);
        if (!need_aac) return;
        dec = avcodec_find_decoder(AV_CODEC_ID_AAC);
        if (!dec) { init_failed = true; return; }
        dctx = avcodec_alloc_context3(dec);
        dctx->sample_rate = TARGET_RATE;
        dctx->ch_layout = AV_CHANNEL_LAYOUT_MONO;
        if (avcodec_open2(dctx, dec, nullptr) < 0) { init_failed = true; return; }
        AVChannelLayout mono = AV_CHANNEL_LAYOUT_MONO;
        if (swr_alloc_set_opts2(&swr, &mono, AV_SAMPLE_FMT_S16, TARGET_RATE,
                                &dctx->ch_layout, dctx->sample_fmt,
                                dctx->sample_rate, 0, nullptr) < 0) {
            init_failed = true; return;
        }
        swr_init(swr);
        frame = av_frame_alloc();
        pkt = av_packet_alloc();
    }
    ~Impl() {
        if (pkt) av_packet_free(&pkt);
        if (frame) av_frame_free(&frame);
        if (swr) swr_free(&swr);
        if (dctx) avcodec_free_context(&dctx);
    }
};

SilenceDetector::SilenceDetector(const ProcessConfig& cfg)
    : cfg_(cfg), impl_(std::make_unique<Impl>(cfg.aac)) {
    ring_.assign(TARGET_RATE * RING_CAP_SECONDS, 0);
    win_samples_ = TARGET_RATE * cfg.silence_window_ms / 1000;   // 320
    step_samples_ = TARGET_RATE * cfg.silence_overlap_ms / 1000; // 160
}

SilenceDetector::~SilenceDetector() = default;

void SilenceDetector::ingest_pcm(const int16_t* samples, size_t n,
                                 int64_t begin_us, uint32_t sample_rate) {
    if (n == 0) return;
    if (!have_window_origin_) {
        have_window_origin_ = true;
        ring_time_head_us_ = begin_us;
        next_window_begin_us_ = begin_us;
    }
    // 写入有界环形缓存；样本时间由 head 与位置线性推算
    for (size_t i = 0; i < n; ++i) {
        size_t idx = (ring_head_ + ring_size_) % ring_.size();
        ring_[idx] = samples[i];
        if (ring_size_ < ring_.size()) ++ring_size_;
        else ring_head_ = (ring_head_ + 1) % ring_.size();
    }
    (void)sample_rate;
    digest_.push_back(rms_milli(samples, n)); // 规范化采样摘要
    drain_windows(false);
}

void SilenceDetector::drain_windows(bool flush) {
    while (ring_size_ >= win_samples_ || (flush && ring_size_ > 0)) {
        if (!flush && ring_size_ < win_samples_) break;
        size_t n = std::min(size_t(win_samples_), ring_size_);
        std::vector<int16_t> tmp(n);
        for (size_t i = 0; i < n; ++i)
            tmp[i] = ring_[(ring_head_ + i) % ring_.size()];
        int64_t w_begin = next_window_begin_us_;
        int64_t w_end = w_begin + int64_t(n) * 1'000'000 / TARGET_RATE;
        // 损坏帧不进入此路径；RMS 低于阈值即静音窗口
        if (rms_milli(tmp.data(), n) < uint32_t(cfg_.silence_threshold_milli)) {
            ++counters_.silence_windows;
            if (!intervals_.empty() &&
                w_begin <= intervals_.back().end_us) {
                // 重叠/相邻窗口合并边界
                intervals_.back().end_us = std::max(intervals_.back().end_us, w_end);
            } else {
                intervals_.push_back({w_begin, w_end});
            }
        }
        if (flush) { ring_size_ = 0; break; }
        // 前移一个步长（窗口重叠 = win - step）
        size_t move = std::min(size_t(step_samples_), ring_size_);
        ring_head_ = (ring_head_ + move) % ring_.size();
        ring_size_ -= move;
        ring_time_head_us_ += int64_t(move) * 1'000'000 / TARGET_RATE;
        next_window_begin_us_ += int64_t(step_samples_) * 1'000'000 / TARGET_RATE;
    }
}

void SilenceDetector::decode_aac(const uint8_t* data, size_t len, uint32_t seq,
                                 int64_t begin_us,
                                 std::vector<PipelineError>& errors) {
    if (!impl_ || impl_->init_failed || !impl_->dctx) {
        ++counters_.corrupt_frames; // 解码器不可用 => 损坏帧，不计静音
        errors.push_back({ErrType::DecodeError, stage::Silence, seq,
                          "aac decoder unavailable"});
        return;
    }
    // 解析 ADTS 头，取出 raw AAC frame（7 字节头，无 CRC）
    if (len < 7 || data[0] != 0xFF || (data[1] & 0xF0) != 0xF0) {
        ++counters_.corrupt_frames;
        errors.push_back({ErrType::DecodeError, stage::Silence, seq,
                          "bad ADTS header; corrupt frame not counted as silence"});
        return;
    }
    impl_->pkt->data = const_cast<uint8_t*>(data) + 7;
    impl_->pkt->size = int(len) - 7;
    int rc = avcodec_send_packet(impl_->dctx, impl_->pkt);
    if (rc < 0) {
        ++counters_.corrupt_frames;
        errors.push_back({ErrType::DecodeError, stage::Silence, seq,
                          "aac send_packet failed; corrupt frame excluded"});
        return;
    }
    while (avcodec_receive_frame(impl_->dctx, impl_->frame) == 0) {
        ++counters_.decoded_frames;
        uint8_t* out_data[1] = {nullptr};
        int out_linesize = 0;
        int dst_samples = av_rescale_rnd(
            swr_get_delay(impl_->swr, impl_->dctx->sample_rate) +
                impl_->frame->nb_samples,
            TARGET_RATE, impl_->dctx->sample_rate, AV_ROUND_UP);
        int av = av_samples_alloc(out_data, &out_linesize, 1, dst_samples,
                                  AV_SAMPLE_FMT_S16, 0);
        if (av < 0) { ++counters_.corrupt_frames; continue; }
        int got = swr_convert(impl_->swr, out_data, dst_samples,
                              impl_->frame->data, impl_->frame->nb_samples);
        if (got > 0)
            ingest_pcm(reinterpret_cast<const int16_t*>(out_data[0]),
                       size_t(got), begin_us, TARGET_RATE);
        av_freep(&out_data[0]);
        av_frame_unref(impl_->frame);
    }
}

void SilenceDetector::push(const MediaPacket& p,
                           std::vector<PipelineError>& errors) {
    if (p.codec == CodecId::PcmS16) {
        size_t n = p.payload.size() / sizeof(int16_t);
        ingest_pcm(reinterpret_cast<const int16_t*>(p.payload.data()),
                   n, p.unified_us, p.clock_rate ? p.clock_rate : TARGET_RATE);
    } else if (p.codec == CodecId::Aac) {
        decode_aac(p.payload.data(), p.payload.size(), p.seq,
                   p.unified_us, errors);
    } else {
        ++counters_.corrupt_frames;
        errors.push_back({ErrType::UnsupportedCodec, stage::Silence, p.seq,
                          "silence stage supports PCM/AAC only"});
    }
}

std::vector<SilenceInterval> SilenceDetector::finish() {
    drain_windows(true);
    return intervals_;
}

} // namespace md
