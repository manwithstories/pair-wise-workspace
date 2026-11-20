#include <filesystem>

#include "mediacore/dash_expand.hpp"
#include "mediacore/dash_template.hpp"
#include "mediacore/pathutil.hpp"

namespace mediacore {

namespace fs = std::filesystem;

void expandRepresentation(Representation& rep, const TmplCtx& ctx,
                          const Timeline& timeline,
                          const fs::path& dir, const fs::path& root,
                          const std::string& trackName,
                          ErrorLog& errors) {
    const Rational tickBase{1, ctx.timescale};
    int64_t n = ctx.startNumber;

    if (timeline.empty() && ctx.duration > 0) {
        // Fixed-duration template without explicit timeline: expand while
        // matching files actually exist on disk.
        for (int guard = 0; guard < 512; ++guard) {
            const int64_t t =
                (n - ctx.startNumber) * ctx.duration;
            const std::string uri =
                substituteTemplate(ctx.media, rep.id, n, t);
            auto resolved = resolveManifestUri(dir, uri, root);
            if (!resolved || !fs::exists(*resolved)) break;
            SegmentRef seg;
            seg.sequence = n;
            seg.duration =
                Rational::reduced(ctx.duration, ctx.timescale);
            seg.startTick = t;
            seg.tickBase = tickBase;
            seg.uri = uri;
            seg.initUri =
                substituteTemplate(ctx.init, rep.id, n, 0);
            rep.segments.push_back(std::move(seg));
            ++n;
        }
        return;
    }

    for (auto [t, d] : timeline) {
        const std::string uri =
            substituteTemplate(ctx.media, rep.id, n, t);
        SegmentRef seg;
        seg.sequence = n++;
        seg.duration = Rational::reduced(d, ctx.timescale);
        seg.startTick = t;
        seg.tickBase = tickBase;
        seg.uri = uri;
        seg.initUri =
            substituteTemplate(ctx.init, rep.id, seg.sequence, t);
        rep.segments.push_back(std::move(seg));
    }

    for (const auto& s : rep.segments) {
        auto resolved = resolveManifestUri(dir, s.uri, root);
        if (!resolved) {
            errors.add({ErrorCode::PathEscape, trackName,
                        static_cast<int>(s.sequence), s.uri,
                        {s.startTick.value_or(0),
                         s.startTick.value_or(0) + s.duration.num,
                         tickBase}, false,
                        "segment URI escapes input directory"});
        } else if (!fs::exists(*resolved)) {
            errors.add({ErrorCode::SegmentMissing, trackName,
                        static_cast<int>(s.sequence), s.uri,
                        {s.startTick.value_or(0),
                         s.startTick.value_or(0) + s.duration.num,
                         tickBase}, true,
                        "declared segment file not found"});
        }
    }
}

}  // namespace mediacore
