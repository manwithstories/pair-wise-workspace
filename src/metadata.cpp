#include <array>
#include <sstream>

extern "C" {
#include <libavutil/pixdesc.h>
#include <libavutil/pixfmt.h>
}

#include "mediacore/metadata.hpp"

namespace mediacore {

const char* colorRangeName(int value) {
    const char* n = av_color_range_name(
        static_cast<AVColorRange>(value));
    return n ? n : "unknown";
}
const char* colorPrimariesName(int value) {
    const char* n = av_color_primaries_name(
        static_cast<AVColorPrimaries>(value));
    return n ? n : "unknown";
}
const char* colorTransferName(int value) {
    const char* n = av_color_transfer_name(
        static_cast<AVColorTransferCharacteristic>(value));
    return n ? n : "unknown";
}
const char* colorMatrixName(int value) {
    const char* n = av_color_space_name(
        static_cast<AVColorSpace>(value));
    return n ? n : "unknown";
}

const char* toString(ColorPolicy policy) {
    switch (policy) {
        case ColorPolicy::Strict: return "strict";
        case ColorPolicy::PreferTag: return "prefer-tag";
        case ColorPolicy::PreferSrgb: return "prefer-srgb";
    }
    return "strict";
}

ColorPolicy parseColorPolicy(const std::string& text) {
    if (text == "prefer-tag") return ColorPolicy::PreferTag;
    if (text == "prefer-srgb") return ColorPolicy::PreferSrgb;
    return ColorPolicy::Strict;
}

namespace {

// One color field handled uniformly: detect unknowns and disagreements.
struct FieldView {
    const char* name;
    int ColorMetadata::*member;
    const char* (*namer)(int);
};

constexpr std::array<FieldView, 4> kFields = {{
    {"range", &ColorMetadata::range, colorRangeName},
    {"primaries", &ColorMetadata::primaries, colorPrimariesName},
    {"transfer", &ColorMetadata::transfer, colorTransferName},
    {"matrix", &ColorMetadata::matrix, colorMatrixName},
}};

int srgbDefaultFor(const FieldView& f) {
    if (f.member == &ColorMetadata::range)
        return AVCOL_RANGE_MPEG;
    if (f.member == &ColorMetadata::primaries)
        return AVCOL_PRI_BT709;
    if (f.member == &ColorMetadata::transfer)
        return AVCOL_TRC_IEC61966_2_1;
    return AVCOL_SPC_BT709;
}

int resolveField(const std::string& track, const FieldView& f,
                 const std::vector<ColorMetadata>& segments,
                 ColorPolicy policy, ColorReport& report,
                 ErrorLog& errors) {
    int known = 0;
    int firstValue = 0;
    int otherValue = 0;
    bool conflict = false;
    for (const auto& s : segments) {
        const int v = s.*f.member;
        if (v == 0) continue;
        if (known == 0) {
            firstValue = v;
        } else if (v != firstValue) {
            conflict = true;
            otherValue = v;
        }
        ++known;
    }
    if (known == 0) {
        report.incomplete = true;
        errors.add({ErrorCode::ColorMetadataMissing, track, -1, "",
                    {0, 0, {1, 1}}, true,
                    std::string("missing color field: ") + f.name});
        return 0;
    }
    if (!conflict) return firstValue;

    int chosen = firstValue;
    std::string rationale = "kept first tagged value";
    if (policy == ColorPolicy::Strict) {
        errors.add({ErrorCode::ColorConflict, track, -1, "",
                    {0, 0, {1, 1}}, false,
                    std::string("conflicting color field: ") + f.name});
        rationale = "strict policy: audit stops";
    } else if (policy == ColorPolicy::PreferSrgb) {
        chosen = srgbDefaultFor(f);
        rationale = std::string("prefer-srgb policy: resolved to ") +
                    f.namer(chosen);
    }
    report.conflicts.push_back(
        ColorConflict{f.name, firstValue, otherValue, rationale});
    return chosen;
}

}  // namespace

ColorReport checkColorMetadata(const std::string& track,
                               const std::vector<ColorMetadata>& segments,
                               ColorPolicy policy, ErrorLog& errors) {
    ColorReport report;
    if (segments.empty()) {
        report.incomplete = true;
        return report;
    }
    report.resolved.range =
        resolveField(track, kFields[0], segments, policy, report, errors);
    report.resolved.primaries =
        resolveField(track, kFields[1], segments, policy, report, errors);
    report.resolved.transfer =
        resolveField(track, kFields[2], segments, policy, report, errors);
    report.resolved.matrix =
        resolveField(track, kFields[3], segments, policy, report, errors);
    return report;
}

std::string toString(const ColorMetadata& c) {
    std::ostringstream ss;
    ss << "range=" << colorRangeName(c.range)
       << " primaries=" << colorPrimariesName(c.primaries)
       << " transfer=" << colorTransferName(c.transfer)
       << " matrix=" << colorMatrixName(c.matrix);
    return ss.str();
}

}  // namespace mediacore
