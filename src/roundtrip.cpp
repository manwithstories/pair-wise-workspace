#include "mediacore/manifest.hpp"

namespace mediacore {

namespace {

void diffField(RoundTripReport& rep, const std::string& track,
               const std::string& field, const std::string& expected,
               const std::string& actual) {
    ++rep.comparedFields;
    if (expected == actual) {
        ++rep.matchedFields;
    } else {
        rep.mismatches.push_back(
            FieldDiff{track, field, expected, actual});
    }
}

std::string rat(const Rational& r) {
    return std::to_string(r.num) + "/" + std::to_string(r.den);
}

void compareSegments(RoundTripReport& rep, const std::string& track,
                     const std::vector<SegmentRef>& a,
                     const std::vector<SegmentRef>& b) {
    diffField(rep, track, "segment_count", std::to_string(a.size()),
              std::to_string(b.size()));
    const std::size_t n = std::min(a.size(), b.size());
    for (std::size_t i = 0; i < n; ++i) {
        const std::string pfx = "seg[" + std::to_string(i) + "].";
        diffField(rep, track, pfx + "sequence",
                  std::to_string(a[i].sequence),
                  std::to_string(b[i].sequence));
        diffField(rep, track, pfx + "duration", rat(a[i].duration),
                  rat(b[i].duration));
        diffField(rep, track, pfx + "uri", a[i].uri, b[i].uri);
        diffField(rep, track, pfx + "init", a[i].initUri, b[i].initUri);
        diffField(rep, track, pfx + "discontinuity",
                  std::to_string(a[i].discontinuity),
                  std::to_string(b[i].discontinuity));
        const bool hasA = a[i].startTick.has_value();
        const bool hasB = b[i].startTick.has_value();
        if (hasA && hasB) {
            diffField(rep, track, pfx + "start_tick",
                      std::to_string(*a[i].startTick),
                      std::to_string(*b[i].startTick));
            diffField(rep, track, pfx + "tick_base",
                      rat(a[i].tickBase), rat(b[i].tickBase));
        } else {
            diffField(rep, track, pfx + "start_tick_present",
                      hasA ? "1" : "0", hasB ? "1" : "0");
        }
    }
}

void compareReps(RoundTripReport& rep, const std::string& track,
                 const Representation& ra, const Representation& rb) {
    diffField(rep, track, "rep.id", ra.id, rb.id);
    diffField(rep, track, "rep.bandwidth",
              std::to_string(ra.bandwidthBps),
              std::to_string(rb.bandwidthBps));
    diffField(rep, track, "rep.content_type", ra.contentType,
              rb.contentType);
    diffField(rep, track, "rep.lang", ra.lang, rb.lang);
    diffField(rep, track, "rep.frame_rate", rat(ra.frameRate),
              rat(rb.frameRate));
    diffField(rep, track, "rep.size",
              std::to_string(ra.width) + "x" + std::to_string(ra.height),
              std::to_string(rb.width) + "x" +
                  std::to_string(rb.height));
    std::string ca, cb;
    for (const auto& c : ra.codecs) { ca += c; ca += ','; }
    for (const auto& c : rb.codecs) { cb += c; cb += ','; }
    diffField(rep, track, "rep.codecs", ca, cb);
    compareSegments(rep, track, ra.segments, rb.segments);
}

void comparePlaylists(RoundTripReport& rep, const MediaPlaylist& a,
                      const MediaPlaylist& b) {
    diffField(rep, a.name, "media_type", a.mediaType, b.mediaType);
    diffField(rep, a.name, "ended", a.ended ? "1" : "0",
              b.ended ? "1" : "0");
    diffField(rep, a.name, "target_duration", rat(a.targetDuration),
              rat(b.targetDuration));
    diffField(rep, a.name, "rep_count",
              std::to_string(a.representations.size()),
              std::to_string(b.representations.size()));
    const std::size_t n =
        std::min(a.representations.size(), b.representations.size());
    for (std::size_t i = 0; i < n; ++i)
        compareReps(rep, a.name, a.representations[i],
                    b.representations[i]);
}

}  // namespace

RoundTripReport roundTripCompare(const Manifest& m) {
    RoundTripReport rep;
    const std::string canonical = serializeManifest(m);
    const Manifest rebuilt = materializeManifest(canonical);

    diffField(rep, "", "format", toString(m.format),
              toString(rebuilt.format));
    diffField(rep, "", "playlist_count",
              std::to_string(m.playlists.size()),
              std::to_string(rebuilt.playlists.size()));
    const std::size_t n =
        std::min(m.playlists.size(), rebuilt.playlists.size());
    for (std::size_t i = 0; i < n; ++i)
        comparePlaylists(rep, m.playlists[i], rebuilt.playlists[i]);

    // Determinism guard: a second serialization must be byte-identical.
    const std::string again = serializeManifest(rebuilt);
    diffField(rep, "", "canonical_determinism",
              std::to_string(canonical.size()),
              std::to_string(again.size()));
    if (canonical != again) {
        rep.mismatches.push_back(
            FieldDiff{"", "canonical_identity", "same", "different"});
        ++rep.comparedFields;
    } else {
        ++rep.comparedFields;
        ++rep.matchedFields;
    }
    return rep;
}

}  // namespace mediacore
