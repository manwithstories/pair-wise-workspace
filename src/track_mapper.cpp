#include "mediacore/track_mapper.hpp"

#include <cstring>
#include <unordered_set>

namespace mediacore {

const char* trackLayoutName(TrackLayout layout) noexcept {
    switch (layout) {
    case TrackLayout::Mono:   return "mono";
    case TrackLayout::Stereo: return "stereo";
    }
    return "unknown";
}

Result<void> TrackMapper::applyConfig(const MappingConfig& config,
                                      const PcmFormat& inputFormat) {
    if (!inputFormat.valid()) {
        return MediaError{MediaErrorCode::UnsupportedFormat,
                          "input PCM format invalid", false};
    }
    if (config.tracks.empty()) {
        return MediaError{MediaErrorCode::InvalidArgument,
                          "mapping must define at least one output track",
                          false};
    }

    std::unordered_set<int> used;
    for (const auto& tr : config.tracks) {
        if (tr.name.empty()) {
            return MediaError{MediaErrorCode::InvalidArgument,
                              "output track name must not be empty", false};
        }
        const int expected = tr.layout == TrackLayout::Stereo ? 2 : 1;
        if (static_cast<int>(tr.inputChannels.size()) != expected) {
            return MediaError{MediaErrorCode::MappingIncompatible,
                              "track '" + tr.name + "' layout " +
                                  trackLayoutName(tr.layout) +
                                  " requires " + std::to_string(expected) +
                                  " input channels, got " +
                                  std::to_string(tr.inputChannels.size()),
                              false};
        }
        for (int ch : tr.inputChannels) {
            if (ch < 0 || ch >= inputFormat.channels) {
                return MediaError{MediaErrorCode::MappingOutOfRange,
                                  "track '" + tr.name +
                                      "' references input channel " +
                                      std::to_string(ch) +
                                      " but input has " +
                                      std::to_string(inputFormat.channels) +
                                      " channels",
                                  false};
            }
            if (!used.insert(ch).second) {
                return MediaError{MediaErrorCode::MappingDuplicate,
                                  "input channel " + std::to_string(ch) +
                                      " occupied by multiple tracks",
                                  false};
            }
        }
    }
    config_ = config;
    tracks_ = config.tracks;
    inputFormat_ = inputFormat;
    return Result<void>{};
}

std::vector<MappedBlock> TrackMapper::mapBlock(const AudioBlock& in) const {
    std::vector<MappedBlock> out;
    out.reserve(tracks_.size());
    for (const auto& tr : tracks_) {
        MappedBlock mb;
        mb.trackName = tr.name;
        mb.layout = tr.layout;
        mb.startSample = in.startSample;
        mb.kind = in.kind;
        mb.format = inputFormat_;
        mb.format.channels =
            tr.layout == TrackLayout::Stereo ? 2 : 1;

        if (in.kind == BlockKind::Corrupt || in.kind == BlockKind::Gap) {
            out.push_back(std::move(mb));
            continue;
        }

        const int inCh = inputFormat_.channels;
        const SampleTick frames = in.frameCount();
        const std::size_t outFrameBytes = mb.format.frameBytes();
        mb.interleavedPcm.resize(
            static_cast<std::size_t>(frames) * outFrameBytes);
        const auto* src =
            reinterpret_cast<const std::int16_t*>(in.interleavedPcm.data());
        auto* dst =
            reinterpret_cast<std::int16_t*>(mb.interleavedPcm.data());
        for (SampleTick f = 0; f < frames; ++f) {
            for (std::size_t k = 0; k < tr.inputChannels.size(); ++k) {
                dst[f * tr.inputChannels.size() + k] =
                    src[f * inCh + tr.inputChannels[k]];
            }
        }
        out.push_back(std::move(mb));
    }
    return out;
}

} // namespace mediacore
