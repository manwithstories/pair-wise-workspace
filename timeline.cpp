#include "timeline.hpp"

#include "json.hpp"
#include "transition_kernel.hpp"

#include <algorithm>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <limits>
#include <sstream>
#include <stdexcept>

namespace bake {

namespace {

[[noreturn]] void timelineError(const std::string& message, int64_t frame = -1) {
    throw BakeError(ExitCode::invalid_input,
                    message + locationSuffix(frame), frame);
}

const Json& requireField(const Json& object, const char* key) {
    const Json* value = object.find(key);
    if (value == nullptr) {
        timelineError(std::string("timeline is missing field: ") + key);
    }
    return *value;
}

int64_t requireInteger(const Json& object, const char* key) {
    const Json& value = requireField(object, key);
    if (!value.isInteger()) {
        timelineError(std::string("timeline field must be an integer: ") + key);
    }
    return value.int_value;
}

std::string requireString(const Json& object, const char* key) {
    const Json& value = requireField(object, key);
    if (!value.isString()) {
        timelineError(std::string("timeline field must be a string: ") + key);
    }
    return value.string_value;
}

const Json& requireArray(const Json& object, const char* key) {
    const Json& value = requireField(object, key);
    if (!value.isArray()) {
        timelineError(std::string("timeline field must be an array: ") + key);
    }
    return value;
}

bool safeRelativePath(const std::filesystem::path& path) {
    if (path.empty() || path.is_absolute()) return false;
    for (const auto& component : path) {
        if (component == "..") return false;
    }
    return true;
}

std::string readTextFile(const std::string& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        timelineError("cannot open timeline: " + path);
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    if (!input.good() && !input.eof()) {
        timelineError("failed reading timeline: " + path);
    }
    return buffer.str();
}

}  // namespace

std::string frameFileName(int64_t frame) {
    if (frame < 0) {
        timelineError("negative output frame number", frame);
    }
    std::ostringstream name;
    name << std::setw(6) << std::setfill('0') << frame << ".ppm";
    return name.str();
}

Timeline Timeline::load(const std::string& path) {
    Timeline result;
    try {
        const Json root = parseJson(readTextFile(path));
        if (!root.isObject()) {
            timelineError("timeline root must be an object");
        }
        const int64_t width = requireInteger(root, "width");
        const int64_t height = requireInteger(root, "height");
        if (width <= 0 || height <= 0 || width > 32768 || height > 32768) {
            timelineError("timeline frame dimensions are invalid");
        }
        result.width = static_cast<int>(width);
        result.height = static_cast<int>(height);
        const int64_t seed = requireInteger(root, "seed");
        if (seed < 0) timelineError("timeline seed must be non-negative");
        result.seed = static_cast<uint64_t>(seed);

        const Json& speed_json = requireArray(root, "speed");
        for (const Json& item : speed_json.array_value) {
            if (!item.isObject()) timelineError("speed entry must be an object");
            const Json& rate = requireArray(item, "rate");
            if (rate.array_value.size() != 2U || !rate.array_value[0].isInteger() ||
                !rate.array_value[1].isInteger()) {
                timelineError("speed rate must be a two-integer array");
            }
            result.speed.push_back(SpeedInterval{
                requireInteger(item, "frames"), requireInteger(item, "source"),
                rate.array_value[0].int_value, rate.array_value[1].int_value});
        }

        const Json& clips_json = requireArray(root, "clips");
        for (const Json& item : clips_json.array_value) {
            if (!item.isObject()) timelineError("clip entry must be an object");
            ClipSpec clip;
            clip.id = requireString(item, "id");
            clip.directory = requireString(item, "dir");
            clip.frames = requireInteger(item, "frames");
            clip.start = requireInteger(item, "start");
            result.clips.push_back(std::move(clip));
        }

        const Json& transitions_json = requireArray(root, "transitions");
        for (const Json& item : transitions_json.array_value) {
            if (!item.isObject()) timelineError("transition entry must be an object");
            TransitionSpec transition;
            transition.from = static_cast<int>(requireInteger(item, "from"));
            transition.to = static_cast<int>(requireInteger(item, "to"));
            transition.kernel = requireString(item, "class");
            transition.start = requireInteger(item, "start");
            transition.frames = requireInteger(item, "frames");
            transition.priority = static_cast<int>(requireInteger(item, "priority"));
            result.transitions.push_back(std::move(transition));
        }
        result.validate();
    } catch (const BakeError&) {
        throw;
    } catch (const std::exception& error) {
        timelineError(std::string("invalid timeline JSON: ") + error.what());
    }
    return result;
}

void Timeline::validate() {
    if (clips.empty()) timelineError("timeline has no clips");
    if (speed.empty()) timelineError("timeline has no speed intervals");
    if (width <= 0 || height <= 0) timelineError("timeline dimensions are invalid");
    if (clips.front().start != 0) {
        timelineError("first clip must start at frame zero");
    }

    int64_t speed_frames = 0;
    for (const SpeedInterval& interval : speed) {
        speed_frames = checkedAdd(speed_frames, interval.output_frames,
                                  "speed schedule length");
    }
    if (speed_frames != clips.front().frames) {
        timelineError("speed schedule length does not match clip length");
    }

    for (std::size_t i = 0; i < clips.size(); ++i) {
        const ClipSpec& clip = clips[i];
        if (clip.id.empty() || clip.directory.empty()) {
            timelineError("clip id and directory must be non-empty");
        }
        if (clip.frames <= 0 || clip.start < 0) {
            timelineError("clip range is invalid", clip.start);
        }
        if (!safeRelativePath(clip.directory)) {
            timelineError("clip directory must be relative and cannot contain ..", clip.start);
        }
        for (std::size_t j = i + 1; j < clips.size(); ++j) {
            if (clips[j].id == clip.id || clips[j].directory == clip.directory) {
                timelineError("duplicate clip id or directory", clip.start);
            }
        }
        if (i > 0 && clip.start <= clips[i - 1].start) {
            timelineError("clip start frames must increase", clip.start);
        }
        checkedAdd(clip.start, clip.frames, "clip end frame");
    }

    if (transitions.size() + 1 != clips.size()) {
        timelineError("timeline requires one transition per adjacent clip pair");
    }
    for (std::size_t i = 0; i < transitions.size(); ++i) {
        const TransitionSpec& transition = transitions[i];
        if (transition.from != static_cast<int>(i) ||
            transition.to != static_cast<int>(i + 1)) {
            timelineError("transition endpoints are invalid", transition.start);
        }
        const ClipSpec& from = clips[static_cast<std::size_t>(transition.from)];
        const ClipSpec& to = clips[static_cast<std::size_t>(transition.to)];
        const int64_t overlap_end = checkedAdd(from.start, from.frames,
                                               "transition overlap");
        const int64_t overlap = checkedAdd(overlap_end, -to.start,
                                           "transition overlap length");
        if (overlap <= 0 || overlap != transition.frames ||
            transition.start != to.start) {
            timelineError("transition window does not match clip overlap",
                          transition.start);
        }
        if (transition.frames < 2) {
            timelineError("transition must contain at least two frames",
                          transition.start);
        }
        if (findKernel(transition.kernel) == nullptr) {
            timelineError("unknown transition kernel: " + transition.kernel,
                          transition.start);
        }
        for (std::size_t j = 0; j < i; ++j) {
            if (transitions[j].priority == transition.priority) {
                timelineError("transition priority conflict at segment " +
                                  std::to_string(transition.to),
                              transition.start);
            }
        }
        for (std::size_t j = i + 1; j < transitions.size(); ++j) {
            const TransitionSpec& other = transitions[j];
            const int64_t a_end = checkedAdd(transition.start, transition.frames,
                                             "transition range");
            const int64_t b_end = checkedAdd(other.start, other.frames,
                                             "transition range");
            if (transition.start < b_end && other.start < a_end) {
                timelineError("transition priority conflict at segment " +
                                  std::to_string(transition.to) + " and " +
                                  std::to_string(other.to),
                              other.start);
            }
        }
    }

    for (const ClipSpec& clip : clips) {
        time_maps.emplace_back(speed, clip.frames - 1, true);
    }
}

int64_t Timeline::totalFrames() const {
    const ClipSpec& last = clips.back();
    return checkedAdd(last.start, last.frames, "timeline frame count");
}

const TransitionSpec* Timeline::transitionAt(int64_t frame) const {
    for (const TransitionSpec& transition : transitions) {
        if (frame >= transition.start &&
            frame < transition.start + transition.frames) {
            return &transition;
        }
    }
    return nullptr;
}

bool Timeline::isIntentionalHold(int64_t frame) const {
    if (frame <= 0 || frame >= totalFrames()) return false;
    std::size_t active_now = clips.size();
    std::size_t active_previous = clips.size();
    for (std::size_t i = 0; i < clips.size(); ++i) {
        if (frame >= clips[i].start && frame < clips[i].start + clips[i].frames) {
            active_now = i;
        }
        if (frame - 1 >= clips[i].start &&
            frame - 1 < clips[i].start + clips[i].frames) {
            active_previous = i;
        }
    }
    if (active_now == clips.size() || active_now != active_previous) return false;
    const TimePoint now = mapClipFrame(active_now, frame);
    const TimePoint before = mapClipFrame(active_previous, frame - 1);
    return now.source_frame == before.source_frame &&
           now.progress_q16 == before.progress_q16;
}

TimePoint Timeline::mapClipFrame(std::size_t clip_index, int64_t frame) const {
    if (clip_index >= clips.size() || clip_index >= time_maps.size()) {
        timelineError("clip index is out of range", frame);
    }
    const int64_t local = frame - clips[clip_index].start;
    if (local < 0 || local >= clips[clip_index].frames) {
        timelineError("clip-local frame is out of range", frame);
    }
    return time_maps[clip_index].map(local);
}

std::vector<LoadedClip> Timeline::loadSources(const std::string& source_root,
                                              bool continue_on_error) const {
    const std::filesystem::path root(source_root);
    std::vector<LoadedClip> loaded;
    loaded.reserve(clips.size());
    for (const ClipSpec& clip : clips) {
        LoadedClip item;
        item.spec = clip;
        const std::filesystem::path directory = root / clip.directory;
        item.frames.reserve(static_cast<std::size_t>(clip.frames));
        item.frame_errors.resize(static_cast<std::size_t>(clip.frames));
        for (int64_t index = 0; index < clip.frames; ++index) {
            const std::filesystem::path path = directory / frameFileName(index);
            FrameBuffer frame(width, height);
            try {
                frame = readPpm(path.string(), clip.start + index);
                if (frame.width != width || frame.height != height) {
                    timelineError("source frame dimensions do not match timeline",
                                  clip.start + index);
                }
            } catch (const BakeError& error) {
                if (!continue_on_error) throw;
                item.frame_errors[static_cast<std::size_t>(index)] = error.what();
            }
            item.frames.push_back(std::move(frame));
        }
        loaded.push_back(std::move(item));
    }
    return loaded;
}

void Timeline::renderFrame(int64_t frame, const std::vector<LoadedClip>& sources,
                           FrameBuffer& destination) const {
    if (frame < 0 || frame >= totalFrames()) {
        timelineError("output frame is out of range", frame);
    }
    if (destination.width != width || destination.height != height) {
        throw BakeError(ExitCode::invariant,
                        "render destination dimensions do not match timeline",
                        frame);
    }
    std::vector<std::size_t> active;
    for (std::size_t i = 0; i < clips.size(); ++i) {
        if (frame >= clips[i].start && frame < clips[i].start + clips[i].frames) {
            active.push_back(i);
        }
    }
    if (active.empty()) {
        timelineError("no clip covers output frame", frame);
    }
    if (active.size() > 2U) {
        timelineError("transition priority conflict at segment " +
                          std::to_string(active[2] + 1),
                      frame);
    }
    if (active.size() == 2U) {
        const TransitionSpec* transition = transitionAt(frame);
        if (transition == nullptr || transition->from != static_cast<int>(active[0]) ||
            transition->to != static_cast<int>(active[1])) {
            timelineError("overlapping clips have no matching transition", frame);
        }
        const TimePoint front_point = mapClipFrame(active[0], frame);
        const TimePoint back_point = mapClipFrame(active[1], frame);
        const LoadedClip& front_clip = sources[active[0]];
        const LoadedClip& back_clip = sources[active[1]];
        if (!front_clip.frame_errors[static_cast<std::size_t>(
                front_point.source_frame)].empty()) {
            throw BakeError(ExitCode::invalid_input,
                            front_clip.frame_errors[static_cast<std::size_t>(
                                front_point.source_frame)], frame);
        }
        if (!back_clip.frame_errors[static_cast<std::size_t>(
                back_point.source_frame)].empty()) {
            throw BakeError(ExitCode::invalid_input,
                            back_clip.frame_errors[static_cast<std::size_t>(
                                back_point.source_frame)], frame);
        }
        const FrameBuffer& front = front_clip.frames[static_cast<std::size_t>(
            front_point.source_frame)];
        const FrameBuffer& back = back_clip.frames[static_cast<std::size_t>(
            back_point.source_frame)];
        const uint64_t local = static_cast<uint64_t>(frame - transition->start);
        const uint32_t progress = static_cast<uint32_t>(
            (local * 65536ULL) / static_cast<uint64_t>(transition->frames - 1));
        const KernelParams params{seed ^ (static_cast<uint64_t>(transition->to) << 32),
                                  frame};
        applyKernel(transition->kernel, front, back, destination, progress, params);
        return;
    }
    const TimePoint point = mapClipFrame(active[0], frame);
    const LoadedClip& clip = sources[active[0]];
    if (!clip.frame_errors[static_cast<std::size_t>(point.source_frame)].empty()) {
        throw BakeError(ExitCode::invalid_input,
                        clip.frame_errors[static_cast<std::size_t>(
                            point.source_frame)], frame);
    }
    const FrameBuffer& source = clip.frames[static_cast<std::size_t>(point.source_frame)];
    if (point.progress_q16 != 0 &&
        point.source_frame + 1 < static_cast<int64_t>(clip.frames.size())) {
        const FrameBuffer& next =
            clip.frames[static_cast<std::size_t>(point.source_frame + 1)];
        applyKernel("linear_blend", source, next, destination, point.progress_q16,
                    KernelParams{seed, frame});
    } else {
        copyFrame(source, destination);
    }
}

namespace {

uint32_t probeProgress(const std::string& kernel, int frame) {
    if (frame <= 1) return 0;
    if (frame >= 22) return 65536;
    const uint32_t linear = static_cast<uint32_t>(
        (static_cast<uint64_t>(frame - 1) * 65536ULL) / 21ULL);
    return kernel == "linear_blend" ? smoothstepQ16(smoothstepQ16(linear)) : linear;
}

void buildProbeFrames(FrameBuffer& front, FrameBuffer& back, uint64_t seed) {
    for (int y = 0; y < front.height; ++y) {
        for (int x = 0; x < front.width; ++x) {
            const uint32_t s = static_cast<uint32_t>(seed);
            const uint8_t ar = static_cast<uint8_t>((x * 5 + y * 3 + s) & 0xffU);
            const uint8_t ag = static_cast<uint8_t>((x * 11 + y * 7 + s * 3U) & 0xffU);
            const uint8_t ab = static_cast<uint8_t>((x * 17 + y * 13 + s * 5U) & 0xffU);
            const uint8_t br = static_cast<uint8_t>(
                (255 - ((x * 7 + y * 2 + s * 11U) & 0xffU)));
            const uint8_t bg = static_cast<uint8_t>(
                ((x / 3 + y / 2) * 29 + s * 7U) & 0xffU);
            const uint8_t bb = static_cast<uint8_t>(
                ((x ^ (y * 3)) * 23 + s * 13U) & 0xffU);
            front.setPixel(x, y, ar, ag, ab);
            back.setPixel(x, y, br, bg, bb);
        }
    }
}

}  // namespace

std::vector<ClassEvidence> computeClassEvidence(int width, int height, uint64_t seed) {
    if (width <= 0 || height <= 0) {
        timelineError("class evidence dimensions are invalid");
    }
    FrameBuffer front(width, height);
    FrameBuffer back(width, height);
    buildProbeFrames(front, back, seed);
    std::vector<ClassEvidence> result;
    for (const KernelEntry& entry : kernelRegistry()) {
        ClassEvidence evidence;
        evidence.class_name = entry.name;
        evidence.frame_count = 24;
        evidence.curve_kind = entry.name == "linear_blend" ? "single_peak" : "plateau";
        FrameBuffer current(width, height);
        FrameBuffer previous(width, height);
        uint64_t total_sum = 0;
        uint64_t total_samples = 0;
        uint64_t peak_mean = 0;
        for (int frame = 0; frame < evidence.frame_count; ++frame) {
            const uint32_t progress = probeProgress(entry.name, frame);
            applyKernel(entry.name, front, back, current, progress,
                        KernelParams{seed, frame});
            if (frame == 0) evidence.first_frame_hash = fnv1a64(current);
            if (frame + 1 == evidence.frame_count) {
                evidence.last_frame_hash = fnv1a64(current);
            }
            if (frame > 0) {
                const FrameDiffStats diff = diffFrames(previous, current);
                evidence.diff_curve.push_back(diff.meanAbsDiff());
                total_sum += diff.sum_abs;
                total_samples += diff.sample_count;
                evidence.max_abs_diff = std::max(evidence.max_abs_diff, diff.max_abs);
                evidence.changed_pixels = std::max(evidence.changed_pixels,
                                                   diff.changed_pixels);
                for (std::size_t bin = 0; bin < kDiffHistogramBins; ++bin) {
                    evidence.diff_histogram[bin] += diff.histogram[bin];
                }
                if (diff.meanAbsDiff() > peak_mean) {
                    peak_mean = diff.meanAbsDiff();
                    evidence.diff_peak_frame = frame;
                }
            }
            previous.copyFrom(current);
        }
        evidence.mean_abs_diff = total_samples == 0
                                     ? 0
                                     : static_cast<uint32_t>(
                                           (total_sum + total_samples / 2) / total_samples);
        result.push_back(std::move(evidence));
    }
    return result;
}

}  // namespace bake


