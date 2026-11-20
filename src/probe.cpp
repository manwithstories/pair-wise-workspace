#include <algorithm>
#include <filesystem>
#include <optional>
#include <unordered_set>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/channel_layout.h>
}

#include "mediacore/probe.hpp"

namespace mediacore {

const char* toString(MediaKind kind) {
    switch (kind) {
        case MediaKind::Video: return "video";
        case MediaKind::Audio: return "audio";
        case MediaKind::Subtitle: return "subtitle";
        case MediaKind::Data: return "data";
        case MediaKind::Unknown: return "unknown";
    }
    return "unknown";
}

namespace {

MediaKind kindOf(AVMediaType t) {
    switch (t) {
        case AVMEDIA_TYPE_VIDEO: return MediaKind::Video;
        case AVMEDIA_TYPE_AUDIO: return MediaKind::Audio;
        case AVMEDIA_TYPE_SUBTITLE: return MediaKind::Subtitle;
        case AVMEDIA_TYPE_DATA: return MediaKind::Data;
        default: return MediaKind::Unknown;
    }
}

Rational rat(AVRational r) {
    return Rational::reduced(r.num, r.den == 0 ? 1 : r.den);
}

StreamInfo describeStream(AVFormatContext* fmt, const AVCodecParameters* p,
                          int index) {
    StreamInfo s;
    s.index = index;
    s.kind = kindOf(p->codec_type);
    const AVCodecDescriptor* d =
        avcodec_descriptor_get(p->codec_id);
    if (d) {
        s.codecName = d->name ? d->name : "";
        s.codecLongName = d->long_name ? d->long_name : "";
    }
    const char* profile =
        avcodec_profile_name(p->codec_id, p->profile);
    if (profile) s.profileName = profile;
    s.bitrate = p->bit_rate;
    if (p->codec_type == AVMEDIA_TYPE_VIDEO) {
        s.width = p->width;
        s.height = p->height;
        s.frameRate = rat(fmt->streams[index]->avg_frame_rate);
        s.sampleAspect = rat(p->sample_aspect_ratio);
        s.colorRange = static_cast<int>(p->color_range);
        s.colorPrimaries = static_cast<int>(p->color_primaries);
        s.colorTransfer = static_cast<int>(p->color_trc);
        s.colorSpace = static_cast<int>(p->color_space);
    }
    if (p->codec_type == AVMEDIA_TYPE_AUDIO) {
        s.sampleRate = p->sample_rate;
        s.channels = p->ch_layout.nb_channels;
        char buf[128] = {0};
        AVChannelLayout copy = p->ch_layout;
        if (av_channel_layout_describe(&copy, buf, sizeof(buf)) > 0)
            s.channelLayout = buf;
    }
    s.timeBase = rat(fmt->streams[index]->time_base);
    s.firstPts = fmt->streams[index]->start_time != AV_NOPTS_VALUE
                     ? fmt->streams[index]->start_time
                     : 0;
    s.durationTicks =
        fmt->streams[index]->duration != AV_NOPTS_VALUE
            ? fmt->streams[index]->duration
            : 0;
    return s;
}

}  // namespace

SegmentProbe probeSegment(const std::filesystem::path& path,
                          const std::string& track, int segmentIndex,
                          const ProbeOptions& opts, ErrorLog& errors) {
    // Keep terminal output clean: corruption is surfaced through our own
    // error model rather than FFmpeg's stderr logging.
    static const int logLevel = (av_log_set_level(AV_LOG_FATAL), 0);
    (void)logLevel;
    SegmentProbe out;
    out.uri = path.string();
    std::error_code ec;
    if (!std::filesystem::is_regular_file(path, ec) ||
        std::filesystem::file_size(path, ec) == 0) {
        out.tailCorrupt = true;
        errors.add({ErrorCode::ZeroLengthRange, track, segmentIndex,
                    path.string(), {0, 0, {1, 1}}, true,
                    "segment missing or zero bytes"});
        return out;
    }
    out.fileBytes = static_cast<int64_t>(std::filesystem::file_size(path));

    AVFormatContext* fmt = nullptr;
    int rc = avformat_open_input(&fmt, path.string().c_str(), nullptr,
                                 nullptr);
    if (rc < 0) {
        char err[AV_ERROR_MAX_STRING_SIZE] = {0};
        av_strerror(rc, err, sizeof(err));
        out.tailCorrupt = true;
        errors.add({ErrorCode::SegmentCorrupt, track, segmentIndex,
                    path.string(), {0, 0, {1, 1}}, true,
                    std::string("avformat_open_input failed: ") + err});
        return out;
    }
    out.opened = true;
    out.containerFormat = fmt->iformat->name ? fmt->iformat->name : "";

    rc = avformat_find_stream_info(fmt, nullptr);
    if (rc < 0) {
        errors.add({ErrorCode::SegmentCorrupt, track, segmentIndex,
                    path.string(), {0, 0, {1, 1}}, true,
                    "stream metadata incomplete"});
    }

    for (unsigned i = 0; i < fmt->nb_streams; ++i) {
        if (!fmt->streams[i] || !fmt->streams[i]->codecpar) continue;
        out.streams.push_back(
            describeStream(fmt, fmt->streams[i]->codecpar,
                           static_cast<int>(i)));
    }

    AVPacket* pkt = av_packet_alloc();
    // PTS may legitimately reorder with B-frames; monotonicity is judged
    // on DTS (decode order) per stream, while duplicate detection uses
    // presentation timestamps.
    std::vector<std::optional<Tick>> lastDts(fmt->nb_streams);
    std::vector<std::optional<Tick>> lastPts(fmt->nb_streams);
    while (opts.maxPacketsPerSegment == 0 ||
           out.packetCount <
               static_cast<int64_t>(opts.maxPacketsPerSegment)) {
        rc = av_read_frame(fmt, pkt);
        if (rc == AVERROR_EOF) break;
        if (rc < 0) {
            out.tailCorrupt = true;
            errors.add({ErrorCode::SegmentCorrupt, track, segmentIndex,
                        path.string(), {0, 0, {1, 1}}, true,
                        "packet read failed: damaged segment tail"});
            break;
        }
        ++out.packetCount;
        if (pkt->stream_index < 0 ||
            pkt->stream_index >= static_cast<int>(lastDts.size()) ||
            !fmt->streams[pkt->stream_index]) {
            av_packet_unref(pkt);
            continue;
        }
        {
            const auto& tb = fmt->streams[pkt->stream_index]->time_base;
            const Rational base = rat(tb);
            const int si = pkt->stream_index;
            if (pkt->dts != AV_NOPTS_VALUE) {
                if (lastDts[si] && pkt->dts < *lastDts[si])
                    out.backwardTimestamps = true;
                lastDts[si] = pkt->dts;
            }
            if (pkt->pts != AV_NOPTS_VALUE) {
                if (lastPts[si] && pkt->pts == *lastPts[si])
                    out.duplicateTimestamps = true;
                lastPts[si] = pkt->pts;
            }
            if (pkt->flags & AV_PKT_FLAG_KEY) {
                out.keyframes.push_back(
                    KeyframePoint{pkt->pts, base,
                                  static_cast<std::size_t>(out.packetCount)});
                out.keyframeBase = base;
            }
        }
        av_packet_unref(pkt);
    }
    av_packet_free(&pkt);

    for (auto& s : out.streams) {
        if (s.index < 0 || s.index >= static_cast<int>(lastPts.size()))
            continue;
        s.lastPts = lastPts[s.index].value_or(s.firstPts);
    }

    // Demuxers (mov/mpegts) commonly turn truncation into clean EOF rather
    // than an error code. Compare the readable span against the declared
    // duration so a damaged tail is still located deterministically.
    if (opts.declaredDuration.num > 0) {
        double bestCoverage = 0.0;
        bool measured = false;
        for (const auto& s : out.streams) {
            if (s.timeBase.den <= 0) continue;
            const double spanSec =
                static_cast<double>(s.lastPts - s.firstPts) *
                s.timeBase.num / s.timeBase.den;
            const double declaredSec =
                static_cast<double>(opts.declaredDuration.num) /
                opts.declaredDuration.den;
            if (declaredSec > 0) {
                bestCoverage =
                    std::max(bestCoverage, spanSec / declaredSec);
                measured = true;
            }
        }
        if (measured && bestCoverage < opts.minDurationCoverage) {
            out.tailCorrupt = true;
            errors.add({ErrorCode::SegmentCorrupt, track, segmentIndex,
                        path.string(), {0, 0, {1, 1}}, true,
                        "readable span shorter than declared duration: "
                        "truncated or corrupt segment tail"});
        }
    }
    if (out.backwardTimestamps)
        errors.add({ErrorCode::TimestampBackward, track, segmentIndex,
                    path.string(), {0, 0, {1, 1}}, true,
                    "non-monotonic packet PTS"});
    if (out.duplicateTimestamps)
        errors.add({ErrorCode::DuplicateSegment, track, segmentIndex,
                    path.string(), {0, 0, {1, 1}}, true,
                    "repeated packet PTS"});
    avformat_close_input(&fmt);
    return out;
}

IndexHitSummary evaluateKeyframeIndex(
    const std::vector<SegmentProbe>& probes,
    const std::vector<TimeRange>& expectedBoundaries) {
    IndexHitSummary sum;
    const std::size_t n = std::min(probes.size(),
                                   expectedBoundaries.size());
    for (std::size_t i = 0; i < n; ++i) {
        if (!probes[i].opened || probes[i].keyframes.empty()) continue;
        ++sum.evaluated;
        // Two valid index-hit interpretations:
        //  (a) standalone file whose boundary is the local origin: the
        //      first keyframe may sit within one frame of PTS zero due to
        //      encoder delay;
        //  (b) an explicit boundary tick that must match exactly after
        //      rescaling (no tolerance, no silent rounding).
        const auto& kf = probes[i].keyframes.front();
        if (expectedBoundaries[i].start == 0) {
            Tick tolerance = 0;
            for (const auto& st : probes[i].streams) {
                if (st.kind == MediaKind::Video && st.frameRate.num > 0) {
                    auto oneFrame = scaleExact(1, st.frameRate, kf.base);
                    if (oneFrame) tolerance = *oneFrame;
                    break;
                }
            }
            if (kf.pts <= tolerance) ++sum.hits;
            continue;
        }
        const auto mapped = scaleExact(kf.pts, kf.base,
                                       expectedBoundaries[i].base);
        if (mapped && *mapped == expectedBoundaries[i].start) ++sum.hits;
    }
    return sum;
}

}  // namespace mediacore
