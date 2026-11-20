#include "mediacore/remux_muxer.hpp"

#include <cstdio>
#include <cstring>
#include <filesystem>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/channel_layout.h>
#include <libavutil/opt.h>
}

namespace mediacore {

namespace fs = std::filesystem;

const char* containerName(Container c) noexcept {
    return c == Container::Wav ? "wav" : "matroska";
}
const char* containerExtension(Container c) noexcept {
    return c == Container::Wav ? ".wav" : ".mka";
}

Result<Container> containerFromPath(const std::string& path) {
    if (path.size() >= 4 &&
        path.compare(path.size() - 4, 4, ".wav") == 0) {
        return Container::Wav;
    }
    if (path.size() >= 4 &&
        path.compare(path.size() - 4, 4, ".mka") == 0) {
        return Container::Matroska;
    }
    return MediaError{MediaErrorCode::UnsupportedFormat,
                      "output must be .wav or .mka: " + path, false};
}

RemuxMuxer::~RemuxMuxer() { abort(); }

Result<void> RemuxMuxer::open(const std::string& finalPath,
                              Container container,
                              const std::vector<OutputTrack>& tracks,
                              int sampleRate) {
    finalPath_ = finalPath;
    tempPath_ = finalPath + ".part";

    const char* fmtName = container == Container::Wav ? "wav" : "matroska";
    int rc = avformat_alloc_output_context2(&fmt_, nullptr, fmtName,
                                            tempPath_.c_str());
    if (rc < 0 || fmt_ == nullptr) {
        char errBuf[64] = {};
        av_make_error_string(errBuf, sizeof(errBuf), rc);
        return MediaError{MediaErrorCode::UnsupportedFormat,
                          std::string("cannot allocate output context: ") +
                              errBuf,
                          false};
    }

    const AVCodec* codec = avcodec_find_encoder(AV_CODEC_ID_PCM_S16LE);
    if (codec == nullptr) {
        return MediaError{MediaErrorCode::UnsupportedFormat,
                          "pcm_s16le encoder identifier unavailable", false};
    }

    for (const auto& tr : tracks) {
        AVStream* st = avformat_new_stream(fmt_, nullptr);
        if (st == nullptr) {
            return MediaError{MediaErrorCode::WriteFailed,
                              "avformat_new_stream failed", false};
        }
        AVCodecParameters* par = st->codecpar;
        par->codec_type = AVMEDIA_TYPE_AUDIO;
        par->codec_id = AV_CODEC_ID_PCM_S16LE;
        par->sample_rate = sampleRate;
        par->format = AV_SAMPLE_FMT_S16;
        const int ch = tr.layout == TrackLayout::Stereo ? 2 : 1;
        if (ch == 2) {
            par->ch_layout = AV_CHANNEL_LAYOUT_STEREO;
        } else {
            par->ch_layout = AV_CHANNEL_LAYOUT_MONO;
        }
        par->bits_per_coded_sample = 16;
        par->block_align = static_cast<int>(ch * 2);
        st->time_base = AVRational{1, sampleRate}; // 采样刻度时基
        streamIndex_.push_back(static_cast<int>(fmt_->nb_streams) - 1);
        trackNames_.push_back(tr.name);
        streamNextSample_.push_back(0);
    }

    if (!(fmt_->oformat->flags & AVFMT_NOFILE)) {
        rc = avio_open(&fmt_->pb, tempPath_.c_str(), AVIO_FLAG_WRITE);
        if (rc < 0) {
            return MediaError{MediaErrorCode::WriteFailed,
                              "avio_open failed for " + tempPath_, false};
        }
    }
    rc = avformat_write_header(fmt_, nullptr);
    if (rc < 0) {
        return MediaError{MediaErrorCode::WriteFailed,
                          "avformat_write_header failed", false};
    }
    headerWritten_ = true;
    return Result<void>{};
}

Result<std::uint64_t> RemuxMuxer::writeBlock(const MappedBlock& block) {
    // 按轨道名定位输出流
    int idx = -1;
    for (std::size_t i = 0; i < trackNames_.size(); ++i) {
        if (trackNames_[i] == block.trackName) {
            idx = streamIndex_[i];
            break;
        }
    }
    if (idx < 0) {
        return MediaError{MediaErrorCode::InvalidArgument,
                          "no output stream for track '" + block.trackName +
                              "'",
                          false};
    }
    int slot = idx; // streamNextSample_ 与 streamIndex_ 平行同值
    int rc = 0;
    AVPacket* pkt = av_packet_alloc();
    if (pkt == nullptr) {
        return MediaError{MediaErrorCode::WriteFailed,
                          "av_packet_alloc failed", false};
    }
    const std::size_t bytes = block.interleavedPcm.size();
    pkt->stream_index = idx;
    pkt->pts = streamNextSample_[slot];
    pkt->dts = pkt->pts;
    pkt->duration = block.format.framesInBytes(
        static_cast<ByteOffset>(bytes));
    streamNextSample_[slot] += pkt->duration;
    pkt->data = const_cast<std::uint8_t*>(block.interleavedPcm.data());
    pkt->size = static_cast<int>(bytes);
    // 让 FFmpeg 内部复制一份可引用计数缓冲，调用方缓冲无需跨调用存活
    rc = av_packet_make_refcounted(pkt);
    if (rc < 0) {
        av_packet_free(&pkt);
        return MediaError{MediaErrorCode::WriteFailed,
                          "av_packet_make_refcounted failed", false};
    }

    rc = av_interleaved_write_frame(fmt_, pkt);
    av_packet_free(&pkt);
    if (rc < 0) {
        return MediaError{MediaErrorCode::WriteFailed,
                          "av_interleaved_write_frame failed", false};
    }
    payloadBytes_ += bytes;
    return bytes;
}

Result<void> RemuxMuxer::commit() {
    if (committed_ || fmt_ == nullptr) {
        return Result<void>{};
    }
    int rc = av_write_trailer(fmt_);
    if (rc < 0) {
        abort();
        return MediaError{MediaErrorCode::WriteFailed,
                          "av_write_trailer failed", false};
    }
    if (!(fmt_->oformat->flags & AVFMT_NOFILE)) {
        avio_closep(&fmt_->pb);
    }
    avformat_free_context(fmt_);
    fmt_ = nullptr;
    committed_ = true;

    std::error_code ec;
    fs::rename(tempPath_, finalPath_, ec);
    if (ec) {
        fs::remove(tempPath_, ec);
        return MediaError{MediaErrorCode::WriteFailed,
                          "atomic rename failed: " + ec.message(), false};
    }
    return Result<void>{};
}

void RemuxMuxer::abort() {
    if (fmt_ != nullptr) {
        if (headerWritten_ &&
            !(fmt_->oformat->flags & AVFMT_NOFILE)) {
            avio_closep(&fmt_->pb);
        }
        avformat_free_context(fmt_);
        fmt_ = nullptr;
    }
    if (!tempPath_.empty()) {
        std::error_code ec;
        fs::remove(tempPath_, ec);
    }
}

} // namespace mediacore
