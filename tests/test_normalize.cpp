#include <catch2/catch_test_macros.hpp>

extern "C" {
#include <libavutil/samplefmt.h>
}

#include "mediacore/normalize.hpp"

using namespace mediacore;

namespace {
AudioSpec spec(int rate, int ch, std::string layout,
               AVSampleFormat fmt = AV_SAMPLE_FMT_FLTP) {
    return AudioSpec{rate, ch, std::move(layout), fmt};
}
}  // namespace

TEST_CASE("conformant audio produces a no-op plan", "[audio]") {
    ErrorLog errors;
    AudioTarget target;  // 48k stereo FLTP
    auto plan = planAudioNormalization("a0", 0,
                                       spec(48000, 2, "stereo"), target,
                                       errors);
    REQUIRE(plan.feasible);
    REQUIRE(plan.layoutVerified);
    REQUIRE(plan.steps.size() == 1);
    REQUIRE(plan.steps.front().action == AudioAction::None);
    REQUIRE_FALSE(errors.hasFatal());
}

TEST_CASE("rate and layout conflicts yield explicit resample steps",
          "[audio]") {
    ErrorLog errors;
    AudioTarget target;
    auto plan = planAudioNormalization(
        "a0", 1, spec(44100, 6, "5.1(side)"), target, errors);
    REQUIRE(plan.feasible);
    bool sawRate = false, sawRemix = false;
    for (const auto& s : plan.steps) {
        if (s.action == AudioAction::ResampleRate) sawRate = true;
        if (s.action == AudioAction::Remix) sawRemix = true;
    }
    REQUIRE(sawRate);
    REQUIRE(sawRemix);
    REQUIRE(plan.estimatedDelaySamples >= 0);
}

TEST_CASE("unknown layout is retained, never guessed", "[audio]") {
    ErrorLog errors;
    AudioTarget target;
    auto plan = planAudioNormalization(
        "a0", 2, spec(48000, 3, ""), target, errors);
    REQUIRE_FALSE(plan.feasible);
    REQUIRE(errors.count(ErrorCode::UnknownChannelLayout) == 1);
    REQUIRE(plan.steps.back().action == AudioAction::Rejected);
    const auto& e = errors.items().front();
    REQUIRE(e.track == "a0");
    REQUIRE(e.segment == 2);
    REQUIRE_FALSE(e.recoverable);

    ErrorLog e2;
    REQUIRE_FALSE(verifyChannelLayout("a0", 0,
                                       spec(48000, 4, "nonsense-9.x"), e2));
    REQUIRE(e2.count(ErrorCode::UnknownChannelLayout) == 1);
}

TEST_CASE("channel count mismatch against layout is fatal", "[audio]") {
    ErrorLog errors;
    AudioTarget target;
    // "stereo" resolves to 2 channels but the spec claims 3.
    auto plan = planAudioNormalization(
        "a0", 0, spec(48000, 3, "stereo"), target, errors);
    REQUIRE_FALSE(plan.feasible);
    REQUIRE(errors.count(ErrorCode::ChannelCountMismatch) == 1);
}

TEST_CASE("intra-track rate conflicts are located per segment",
          "[audio]") {
    ErrorLog errors;
    std::vector<AudioSpec> specs{
        spec(48000, 2, "stereo"), spec(44100, 2, "stereo"),
        spec(48000, 2, "stereo")};
    auto steps = detectAudioConflicts("a0", specs, errors);
    REQUIRE(errors.count(ErrorCode::SampleRateConflict) == 1);
    REQUIRE_FALSE(steps.empty());
    const auto& e = errors.items().front();
    REQUIRE(e.segment == 1);
    REQUIRE(e.recoverable);
}

TEST_CASE("sample format conversion is planned explicitly", "[audio]") {
    ErrorLog errors;
    AudioTarget target;
    auto plan = planAudioNormalization(
        "a0", 0, spec(48000, 2, "stereo", AV_SAMPLE_FMT_S16), target,
        errors);
    REQUIRE(plan.feasible);
    bool sawFormat = false;
    for (const auto& s : plan.steps)
        if (s.action == AudioAction::ConvertFormat) sawFormat = true;
    REQUIRE(sawFormat);
}
