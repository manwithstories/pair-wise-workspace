// 统一有理数时间基准及时间码换算。
// 支持 24、25、30、30000/1001 帧率，日界回绕与重复时间戳；
// 不可换算项记录源轨道与错误类型（ErrorKind::UnconvertibleTimecode）。
#pragma once

#include "media/error_model.hpp"
#include "media/packet.hpp"

#include <cstdint>
#include <cstdio>
#include <optional>
#include <string>

extern "C" {
#include <libavutil/avutil.h>
#include <libavutil/rational.h>
}

namespace media {

// 有理数，始终保持约分形式。
struct Rational {
    int64_t num = 0;
    int64_t den = 1;

    static std::optional<Rational> make(int64_t n, int64_t d) {
        if (d == 0 || n < 0) return std::nullopt;
        int64_t g = gcd(n, d);
        return Rational{n / g, d / g};
    }

    static int64_t gcd(int64_t a, int64_t b) {
        if (a < 0) a = -a;
        if (b < 0) b = -b;
        while (b != 0) {
            int64_t t = a % b;
            a = b;
            b = t;
        }
        return a == 0 ? 1 : a;
    }
};

// 支持的帧率（时钟制式）。
struct FrameRate {
    Rational fps; // 帧/秒
    const char* name;
    bool drop_frame; // 30000/1001 采用丢帧时间码记法
};

inline const FrameRate kFps24{*Rational::make(24, 1), "24", false};
inline const FrameRate kFps25{*Rational::make(25, 1), "25", false};
inline const FrameRate kFps30{*Rational::make(30, 1), "30", false};
inline const FrameRate kFps2997{*Rational::make(30000, 1001), "30000/1001", true};

// 统一主时基：1/90000 秒。
inline constexpr Rational kMasterTimebase{1, kMasterClockHz};

// 以 AVRounding::Near 做有理数换算，饱和于 int64。
inline int64_t rescale(int64_t value, Rational from, Rational to) {
    AVRational f{static_cast<int>(from.num), static_cast<int>(from.den)};
    AVRational t{static_cast<int>(to.num), static_cast<int>(to.den)};
    return av_rescale_q(value, f, t);
}

inline int64_t ms_to_ticks(int64_t ms) { return ms * (kMasterClockHz / 1000); }
inline double ticks_to_ms(int64_t ticks) {
    return static_cast<double>(ticks) * 1000.0 / kMasterClockHz;
}

struct Timecode {
    int hh = 0, mm = 0, ss = 0, ff = 0;
};

// 解析 "HH:MM:SS:FF"（也容忍以 ';' 表示丢帧分隔）。
inline std::optional<Timecode> parse_timecode(const std::string& text) {
    Timecode tc;
    char sep1, sep2, sep3;
    if (std::sscanf(text.c_str(), "%d%c%d%c%d%c%d", &tc.hh, &sep1, &tc.mm, &sep2,
                    &tc.ss, &sep3, &tc.ff) != 7)
        return std::nullopt;
    if ((sep1 != ':' && sep1 != ';') || (sep2 != ':' && sep2 != ';') ||
        (sep3 != ':' && sep3 != ';'))
        return std::nullopt;
    if (tc.hh < 0 || tc.hh > 23 || tc.mm < 0 || tc.mm > 59 || tc.ss < 0 ||
        tc.ss > 59 || tc.ff < 0)
        return std::nullopt;
    return tc;
}

// 单日总帧数（日界回绕的模）。30000/1001 按丢帧制式折算为 30fps 标称帧号。
inline int64_t frames_per_day(const FrameRate& rate) {
    if (rate.drop_frame) return 30LL * 24 * 3600; // 标称帧号空间
    return rate.fps.num / rate.fps.den * 24 * 3600;
}

// 时间码 -> 当日帧号；非法（帧号越界等）返回 nullopt。
inline std::optional<int64_t> timecode_to_frame(const Timecode& tc,
                                                const FrameRate& rate) {
    int nominal = rate.drop_frame ? 30 : static_cast<int>(rate.fps.num / rate.fps.den);
    if (tc.ff >= nominal) return std::nullopt;
    int64_t total =
        ((static_cast<int64_t>(tc.hh) * 60 + tc.mm) * 60 + tc.ss) * nominal + tc.ff;
    if (rate.drop_frame) {
        // 丢帧制式：每分钟跳过 2 个帧号，逢 10 分钟除外。
        int64_t total_minutes = static_cast<int64_t>(tc.hh) * 60 + tc.mm;
        total -= 2 * (total_minutes - total_minutes / 10);
    }
    if (total < 0 || total >= frames_per_day(rate)) return std::nullopt;
    return total;
}

// 帧号 -> 主时基 tick。30000/1001 的真实帧间隔为 1001/30000 秒。
inline int64_t frame_to_ticks(int64_t frame, const FrameRate& rate) {
    Rational frame_dur{rate.fps.den, rate.fps.num};
    return rescale(frame, frame_dur, kMasterTimebase);
}

// 时间码 -> 主时基 tick（当日偏移）。失败时记录错误并返回 nullopt。
inline std::optional<int64_t>
timecode_to_ticks(const Timecode& tc, const FrameRate& rate,
                  ErrorModel* errors = nullptr,
                  const std::string& source_track = "") {
    auto frame = timecode_to_frame(tc, rate);
    if (!frame) {
        if (errors)
            errors->record({ErrorKind::UnconvertibleTimecode, source_track, -1, -1,
                            -1, "timecode out of range for rate " +
                                    std::string(rate.name)});
        return std::nullopt;
    }
    return frame_to_ticks(*frame, rate);
}

// 帧号序列解回绕：相邻帧号差小于 -frames_per_day/2 判定为日界回绕，
// 返回累计帧号（单调不减）。重复时间戳保持原样（差为 0 合法）。
class FrameUnwrapper {
  public:
    explicit FrameUnwrapper(const FrameRate& rate) : mod_(frames_per_day(rate)) {}

    int64_t unwrap(int64_t frame, ErrorModel* errors = nullptr,
                   const std::string& track = "") {
        if (!initialized_) {
            base_ = frame;
            initialized_ = true;
            return frame;
        }
        int64_t prev = base_ + wraps_ * mod_;
        int64_t diff = frame - (prev % mod_);
        if (diff < -mod_ / 2) {
            wraps_++;
            if (errors)
                errors->record({ErrorKind::TimestampWrap, track, -1, -1, -1,
                                "day boundary wrap"});
        } else if (diff == 0 && errors) {
            errors->record({ErrorKind::DuplicateTimestamp, track, -1, -1, -1,
                            "duplicate timestamp"});
        }
        return frame + wraps_ * mod_;
    }

  private:
    int64_t mod_;
    int64_t base_ = 0;
    int64_t wraps_ = 0;
    bool initialized_ = false;
};

// 主时基 tick -> 时间码（取当日帧号后格式化）。
inline Timecode ticks_to_timecode(int64_t ticks, const FrameRate& rate) {
    int64_t frame = rescale(ticks, kMasterTimebase,
                            Rational{rate.fps.den, rate.fps.num});
    int64_t mod = frames_per_day(rate);
    frame = ((frame % mod) + mod) % mod;
    int nominal = rate.drop_frame ? 30 : static_cast<int>(rate.fps.num / rate.fps.den);
    if (rate.drop_frame) {
        // 真实帧号 -> 丢帧制式标称帧号：每 10 分钟块 17982 真实帧，
        // 块内首分钟 1800 帧，其余每分钟 1798 帧（跳过 2 个帧号）。
        const int64_t fp10m = 17982, fp1m = 1798;
        int64_t d = frame / fp10m;
        int64_t m = frame % fp10m;
        int64_t nominal_frame;
        if (m < 1800) {
            nominal_frame = d * 18000 + m;
        } else {
            int64_t m2 = m - 1800;
            nominal_frame = d * 18000 + 1800 + m2 + 2 * (m2 / fp1m + 1);
        }
        frame = nominal_frame;
    }
    Timecode tc;
    tc.ff = static_cast<int>(frame % nominal);
    int64_t secs = frame / nominal;
    tc.ss = static_cast<int>(secs % 60);
    tc.mm = static_cast<int>((secs / 60) % 60);
    tc.hh = static_cast<int>((secs / 3600) % 24);
    return tc;
}

} // namespace media
