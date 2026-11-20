#include <fstream>
#include <utility>

#include "mediacore/dash_expand.hpp"
#include "mediacore/dash_template.hpp"
#include "mediacore/manifest.hpp"
#include "mediacore/xml_lexer.hpp"

namespace mediacore {

namespace {

// Read SegmentTimeline/S entries following an open SegmentTemplate tag.
Timeline readTimeline(XmlLexer& lex, const TmplCtx& ctx) {
    Timeline timeline;
    XmlTag tag;
    int64_t nextTime = 0;
    while (lex.next(tag)) {
        if (tag.closing && tag.name == "SegmentTemplate") break;
        if (tag.closing || tag.name != "S") continue;
        int64_t t = tag.attrs.count("t")
                        ? std::stoll(tag.attrs.at("t"))
                        : nextTime;
        const int64_t d = tag.attrs.count("d")
                              ? std::stoll(tag.attrs.at("d"))
                              : ctx.duration;
        const int r = tag.attrs.count("r")
                          ? std::stoll(tag.attrs.at("r"))
                          : 0;
        for (int k = 0; k <= r; ++k) {
            timeline.emplace_back(t, d);
            t += d;
        }
        nextTime = t;
    }
    return timeline;
}

void applyTemplateAttrs(const XmlTag& tag, TmplCtx& ctx) {
    if (auto v = tag.attrs.find("media"); v != tag.attrs.end())
        ctx.media = v->second;
    if (auto v = tag.attrs.find("initialization"); v != tag.attrs.end())
        ctx.init = v->second;
    if (auto v = tag.attrs.find("timescale"); v != tag.attrs.end())
        ctx.timescale = std::stoll(v->second);
    if (auto v = tag.attrs.find("startNumber"); v != tag.attrs.end())
        ctx.startNumber = std::stoll(v->second);
    if (auto v = tag.attrs.find("duration"); v != tag.attrs.end())
        ctx.duration = std::stoll(v->second);
}

Representation buildRepresentation(const XmlTag& tag,
                                   const std::string& adaptType,
                                   const std::string& adaptLang) {
    Representation rep;
    rep.id = tag.attrs.count("id") ? tag.attrs.at("id") : "rep";
    rep.contentType = adaptType;
    rep.lang = adaptLang;
    if (auto v = tag.attrs.find("bandwidth"); v != tag.attrs.end())
        rep.bandwidthBps = std::stoll(v->second);
    if (auto v = tag.attrs.find("codecs"); v != tag.attrs.end())
        rep.codecs = codecList(v->second);
    if (auto v = tag.attrs.find("width"); v != tag.attrs.end())
        rep.width = std::stoi(v->second);
    if (auto v = tag.attrs.find("height"); v != tag.attrs.end())
        rep.height = std::stoi(v->second);
    if (auto v = tag.attrs.find("frameRate"); v != tag.attrs.end())
        if (auto fr = parseDecimal(v->second)) rep.frameRate = *fr;
    return rep;
}

}  // namespace

Manifest parseDash(const std::filesystem::path& path,
                   const ParseOptions& opts, ErrorLog& errors) {
    Manifest m;
    m.format = ManifestFormat::Dash;
    m.sourcePath = path;
    m.baseUrl = path.parent_path().string();
    const auto dir = path.parent_path();

    std::ifstream in(path, std::ios::binary);
    std::string content((std::istreambuf_iterator<char>(in)),
                        std::istreambuf_iterator<char>());
    if (in.bad()) {
        errors.add({ErrorCode::ManifestTruncated, "", -1, path.string(),
                    {0, 0, {0, 1}}, false, "cannot read MPD"});
        return m;
    }
    XmlLexer lex(content);
    XmlTag tag;
    bool sawRootClose = false;
    TmplCtx inherited;
    Timeline inheritedTimeline;
    std::string adaptType, adaptLang;

    while (lex.next(tag)) {
        if (tag.closing) {
            if (tag.name == "MPD") sawRootClose = true;
            if (tag.name == "AdaptationSet") {
                adaptType.clear();
                adaptLang.clear();
            }
            continue;
        }
        if (tag.name == "AdaptationSet") {
            adaptType = tag.attrs.count("contentType")
                            ? tag.attrs.at("contentType")
                            : tag.attrs.count("mimeType")
                                  ? tag.attrs.at("mimeType")
                                  : "";
            adaptLang = tag.attrs.count("lang") ? tag.attrs.at("lang") : "";
            continue;
        }
        if (tag.name == "SegmentTemplate") {
            applyTemplateAttrs(tag, inherited);
            if (!tag.selfClose)
                inheritedTimeline = readTimeline(lex, inherited);
            continue;
        }
        if (tag.name != "Representation") continue;

        MediaPlaylist pl;
        Representation rep = buildRepresentation(tag, adaptType, adaptLang);
        pl.name = rep.id;
        pl.trackId = rep.id;
        pl.uri = path.filename().string();
        pl.mediaType = adaptType;

        TmplCtx ctx = inherited;
        Timeline timeline = inheritedTimeline;
        int localDepth = 1;  // the Representation element itself
        while (localDepth > 0 && lex.next(tag)) {
            if (tag.closing) {
                --localDepth;
                continue;
            }
            if (!tag.selfClose) ++localDepth;
            if (tag.name == "SegmentTemplate") {
                applyTemplateAttrs(tag, ctx);
                if (!tag.selfClose) timeline = readTimeline(lex, ctx);
            } else if (tag.name == "S") {
                int64_t t = tag.attrs.count("t")
                                ? std::stoll(tag.attrs.at("t"))
                                : (timeline.empty()
                                       ? 0
                                       : timeline.back().first +
                                             timeline.back().second);
                const int64_t d = tag.attrs.count("d")
                                      ? std::stoll(tag.attrs.at("d"))
                                      : ctx.duration;
                timeline.emplace_back(t, d);
            }
        }

        expandRepresentation(rep, ctx, timeline, dir, opts.root, pl.name,
                             errors);
        pl.ended = sawRootClose;
        pl.representations.push_back(std::move(rep));
        m.playlists.push_back(std::move(pl));
    }

    if (!sawRootClose) {
        errors.add({ErrorCode::ManifestTruncated, "", -1, path.string(),
                    {0, 0, {0, 1}}, false,
                    "MPD missing closing </MPD> (truncated)"});
        for (auto& pl : m.playlists) pl.ended = false;
    }
    return m;
}

}  // namespace mediacore
