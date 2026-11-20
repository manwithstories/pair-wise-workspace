#include <algorithm>
#include <set>

extern "C" {
#include <libavutil/channel_layout.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
}

#include "mediacore/normalize.hpp"

namespace mediacore {

namespace {

// Resolve a layout description through libavutil. Empty/unknown names
// fail explicitly so callers never invent a channel mapping.
bool resolveLayout(const std::string& name, AVChannelLayout& out) {
    if (name.empty()) return false;
    AVChannelLayout parsed;
    int rc = av_channel_layout_from_string(&parsed, name.c_str());
    if (rc < 0) return false;
    out = parsed;
    return true;
}

const char* fmtName(AVSampleFormat f) {
    const char* n = av_get_sample_fmt_name(f);
    return n ? n : "none";
}

}  // namespace

std::string AudioSpec::key() const {
    return std::to_string(sampleRate) + "Hz/" + std::to_string(channels) +
           "ch/" + (layout.empty() ? "unknown" : layout) + "/" +
           fmtName(sampleFormat);
}

const char* toString(AudioAction action) {
    switch (action) {
        case AudioAction::None: return "none";
        case AudioAction::ResampleRate: return "resample_rate";
        case AudioAction::Remix: return "remix_layout";
        case AudioAction::ConvertFormat: return "convert_format";
        case AudioAction::Rejected: return "rejected";
    }
    return "unknown";
}

bool verifyChannelLayout(const std::string& track, int segmentIndex,
                         const AudioSpec& spec, ErrorLog& errors) {
    if (spec.layout.empty()) {
        errors.add({ErrorCode::UnknownChannelLayout, track, segmentIndex,
                    "", {0, 0, {1, 1}}, false,
                    "audio stream reports no channel layout"});
        return false;
    }
    AVChannelLayout parsed;
    if (!resolveLayout(spec.layout, parsed)) {
        errors.add({ErrorCode::UnknownChannelLayout, track, segmentIndex,
                    "", {0, 0, {1, 1}}, false,
                    "unresolvable channel layout: " + spec.layout});
        return false;
    }
    const int layoutChannels = parsed.nb_channels;
    const bool countOk = layoutChannels == spec.channels;
    av_channel_layout_uninit(&parsed);
    if (!countOk) {
        errors.add({ErrorCode::ChannelCountMismatch, track, segmentIndex,
                    "", {0, 0, {1, 1}}, false,
                    "layout '" + spec.layout + "' implies " +
                        std::to_string(layoutChannels) +
                        " channels but stream reports " +
                        std::to_string(spec.channels)});
        return false;
    }
    return true;
}

ResamplePlan planAudioNormalization(const std::string& track,
                                    int segmentIndex,
                                    const AudioSpec& source,
                                    const AudioTarget& target,
                                    ErrorLog& errors) {
    ResamplePlan plan;
    plan.track = track;
    plan.source = source;
    plan.target = AudioSpec{target.sampleRate, target.channels,
                            target.layout, target.sampleFormat};

    if (!verifyChannelLayout(track, segmentIndex, source, errors)) {
        plan.steps.push_back(
            {AudioAction::Rejected,
             "source layout unknown or channel count mismatch"});
        return plan;
    }
    AVChannelLayout srcLayout{};
    AVChannelLayout dstLayout{};
    const bool srcOk = resolveLayout(source.layout, srcLayout);
    const bool dstOk = resolveLayout(target.layout, dstLayout);
    if (!srcOk || !dstOk) {
        plan.steps.push_back(
            {AudioAction::Rejected, "layout resolution failed"});
        if (srcOk) av_channel_layout_uninit(&srcLayout);
        if (dstOk) av_channel_layout_uninit(&dstLayout);
        return plan;
    }

    if (source.sampleRate != target.sampleRate)
        plan.steps.push_back(
            {AudioAction::ResampleRate,
             std::to_string(source.sampleRate) + " -> " +
                 std::to_string(target.sampleRate) + " Hz"});
    if (source.channels != target.channels ||
        source.layout != target.layout)
        plan.steps.push_back(
            {AudioAction::Remix,
             source.layout + " (" + std::to_string(source.channels) +
                 "ch) -> " + target.layout + " (" +
                 std::to_string(target.channels) + "ch)"});
    if (source.sampleFormat != target.sampleFormat)
        plan.steps.push_back(
            {AudioAction::ConvertFormat,
             std::string(fmtName(source.sampleFormat)) + " -> " +
                 fmtName(target.sampleFormat)});
    if (plan.steps.empty())
        plan.steps.push_back({AudioAction::None, "already conformant"});

    // Validate the full chain by actually constructing the swresample
    // context; an infeasible mapping is rejected rather than assumed.
    SwrContext* swr = nullptr;
    int rc = swr_alloc_set_opts2(
        &swr, &dstLayout, target.sampleFormat, target.sampleRate,
        &srcLayout,
        source.sampleFormat == AV_SAMPLE_FMT_NONE ? AV_SAMPLE_FMT_FLTP
                                                  : source.sampleFormat,
        source.sampleRate == 0 ? target.sampleRate : source.sampleRate,
        0, nullptr);
    if (rc < 0 || !swr || (rc = swr_init(swr)) < 0) {
        plan.steps.push_back(
            {AudioAction::Rejected, "swresample rejected the chain"});
        errors.add({ErrorCode::SampleRateConflict, track, segmentIndex,
                    "", {0, 0, {1, 1}}, false,
                    "infeasible audio conversion plan"});
    } else {
        plan.feasible = true;
        plan.layoutVerified = true;
        if (source.sampleRate != target.sampleRate) {
            const int64_t delaySrc = swr_get_delay(
                swr, source.sampleRate == 0 ? target.sampleRate
                                            : source.sampleRate);
            plan.estimatedDelaySamples = av_rescale_rnd(
                delaySrc, target.sampleRate,
                source.sampleRate == 0 ? target.sampleRate
                                       : source.sampleRate,
                AV_ROUND_UP);
        }
    }
    if (swr) swr_free(&swr);
    av_channel_layout_uninit(&srcLayout);
    av_channel_layout_uninit(&dstLayout);
    return plan;
}

std::vector<ResampleStep> detectAudioConflicts(
    const std::string& track, const std::vector<AudioSpec>& specs,
    ErrorLog& errors) {
    std::vector<ResampleStep> steps;
    if (specs.size() < 2) return steps;
    const AudioSpec& anchor = specs.front();
    std::set<int> badSegments;
    for (std::size_t i = 1; i < specs.size(); ++i) {
        if (specs[i].sampleRate != anchor.sampleRate) {
            errors.add({ErrorCode::SampleRateConflict, track,
                        static_cast<int>(i), "", {0, 0, {1, 1}}, true,
                        "segment sample rate " +
                            std::to_string(specs[i].sampleRate) +
                            " differs from anchor " +
                            std::to_string(anchor.sampleRate)});
            badSegments.insert(static_cast<int>(i));
        }
        if (specs[i].channels != anchor.channels) {
            errors.add({ErrorCode::ChannelCountMismatch, track,
                        static_cast<int>(i), "", {0, 0, {1, 1}}, true,
                        "segment channel count differs from anchor"});
            badSegments.insert(static_cast<int>(i));
        }
        if (!specs[i].layout.empty() &&
            specs[i].layout != anchor.layout)
            steps.push_back(
                {AudioAction::Remix,
                 "segment " + std::to_string(i) + " layout " +
                     specs[i].layout + " vs anchor " + anchor.layout});
    }
    if (!badSegments.empty())
        steps.push_back({AudioAction::ResampleRate,
                         "normalize conflicting segments to anchor"});
    return steps;
}

}  // namespace mediacore
