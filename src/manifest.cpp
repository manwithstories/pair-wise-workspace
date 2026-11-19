#include "media/manifest.hpp"
#include <algorithm>
#include <sstream>

namespace media {
const TrackSpec* Manifest::findTrack(const std::string& id) const {
  for (const auto& t : tracks)
    if (t.id == id) return &t;
  return nullptr;
}

namespace {
std::vector<std::string> tokenize(const std::string& line) {
  std::istringstream iss(line);
  std::vector<std::string> tok;
  std::string w;
  while (iss >> w) tok.push_back(w);
  return tok;
}

long long toInt(const std::string& s, bool& ok) {
  try {
    size_t pos = 0;
    long long v = std::stoll(s, &pos);
    ok = pos == s.size();
    return v;
  } catch (...) {
    ok = false;
    return 0;
  }
}
} // namespace

ManifestError parseManifest(const std::string& text, Manifest& out) {
  std::istringstream iss(text);
  std::string line;
  size_t lineNo = 0;
  while (std::getline(iss, line)) {
    ++lineNo;
    auto tok = tokenize(line);
    if (tok.empty() || tok[0].starts_with('#')) continue;

    if (tok[0] == "TRACK") {
      if (tok.size() != 6)
        return {lineNo, "TRACK expects: TRACK <id> <video|audio> <file> <num> <den>"};
      TrackSpec t;
      t.id = tok[1];
      t.type = tok[2] == "audio" ? MediaType::Audio : MediaType::Video;
      if (tok[2] != "video" && tok[2] != "audio")
        return {lineNo, "track type must be video or audio"};
      t.file = tok[3];
      bool ok1, ok2;
      t.timeBase.num = toInt(tok[4], ok1);
      t.timeBase.den = toInt(tok[5], ok2);
      if (!ok1 || !ok2 || t.timeBase.num <= 0 || t.timeBase.den <= 0)
        return {lineNo, "invalid time base rational"};
      out.tracks.push_back(t);
    } else if (tok[0] == "EDIT") {
      if (tok.size() != 4) return {lineNo, "EDIT expects: EDIT <n> <track> <ms>"};
      EditPoint e;
      bool ok;
      e.index = size_t(toInt(tok[1], ok));
      if (!ok) return {lineNo, "invalid edit index"};
      e.trackId = tok[2];
      long long ms = toInt(tok[3], ok);
      if (!ok || ms < 0) return {lineNo, "invalid edit target ms"};
      e.targetUs = ms * 1000;
      out.edits.push_back(e);
    } else if (tok[0] == "XFADE") {
      if (tok.size() != 6)
        return {lineNo, "XFADE expects: XFADE <n> <from> <to> <start_ms> <end_ms>"};
      Transition x;
      bool ok;
      x.editIndex = size_t(toInt(tok[1], ok));
      x.fromTrack = tok[2];
      x.toTrack = tok[3];
      long long a = toInt(tok[4], ok);
      long long b = toInt(tok[5], ok);
      if (!ok || a < 0 || b <= a) return {lineNo, "invalid xfade interval"};
      x.startUs = a * 1000;
      x.endUs = b * 1000;
      out.transitions.push_back(x);
    } else {
      return {lineNo, "unknown directive: " + tok[0]};
    }
  }
  for (const auto& e : out.edits)
    if (!out.findTrack(e.trackId))
      return {lineNo, "EDIT references unknown track: " + e.trackId};
  for (const auto& x : out.transitions)
    if (!out.findTrack(x.fromTrack) || !out.findTrack(x.toTrack))
      return {lineNo, "XFADE references unknown track"};
  return {};
}

std::string manifestToText(const Manifest& m) {
  std::ostringstream o;
  for (const auto& t : m.tracks)
    o << "TRACK " << t.id << ' ' << toString(t.type) << ' ' << t.file << ' '
      << t.timeBase.num << ' ' << t.timeBase.den << '\n';
  for (const auto& e : m.edits)
    o << "EDIT " << e.index << ' ' << e.trackId << ' '
      << e.targetUs / 1000 << '\n';
  for (const auto& x : m.transitions)
    o << "XFADE " << x.editIndex << ' ' << x.fromTrack << ' ' << x.toTrack
      << ' ' << x.startUs / 1000 << ' ' << x.endUs / 1000 << '\n';
  return o.str();
}

BoundaryState alignEditPoint(const EditPoint& edit,
                             const std::vector<MediaPacket>& packets) {
  BoundaryState st;
  st.editIndex = edit.index;
  st.targetUs = edit.targetUs;
  if (packets.empty()) {
    st.kind = BoundaryKind::MissingFrame;
    return st;
  }
  // 不晚于目标时间的最近有效帧；无满足项则取首帧并标记缺帧。
  const MediaPacket* best = nullptr;
  for (const auto& p : packets) {
    if (!p.deliverable) continue;
    if (p.repairedUs <= edit.targetUs) best = &p;
    else break;
  }
  if (!best) {
    best = &packets.front();
    st.kind = BoundaryKind::MissingFrame;
  }
  st.nearestPacket = best->index;
  st.actualUs = best->repairedUs;
  st.errorUs = st.actualUs - st.targetUs;
  if (st.kind != BoundaryKind::MissingFrame)
    st.kind = st.errorUs == 0 ? BoundaryKind::Exact : BoundaryKind::Snapped;
  return st;
}
} // namespace media
