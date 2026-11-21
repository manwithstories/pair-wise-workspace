#include "relay/checkpoint_store.hpp"

#include <cerrno>
#include <cstdio>
#include <cstring>
#include <fcntl.h>
#include <fstream>
#include <sstream>
#include <sys/stat.h>
#include <unistd.h>

#include "relay/sha256.hpp"

namespace relay {

namespace {
constexpr const char* kMagic = "relay-checkpoint-v1";

std::string sanitize(std::string s) {
  for (char& c : s)
    if (c == ' ' || c == '\t' || c == '\n' || c == '\r') c = '_';
  return s;
}

std::string serialize(const Checkpoint& c) {
  std::ostringstream os;
  os << kMagic << "\n";
  os << "input_sha256 " << c.input_sha256 << "\n";
  os << "analysis_version " << c.analysis_version << "\n";
  os << "cursor " << c.cursor << "\n";
  os << "duplicate_packets " << c.duplicate_packets << "\n";
  os << "reordered_packets " << c.reordered_packets << "\n";
  os << "lost_packets " << c.lost_packets << "\n";
  os << "burst_losses " << c.burst_losses << "\n";
  for (const auto& r : c.completed_ranges) {
    os << "range " << r.range.start_ms.num << "/" << r.range.start_ms.den << " "
       << r.range.end_ms.num << "/" << r.range.end_ms.den << " "
       << to_string(r.kind) << " " << to_string(r.severity) << " "
       << sanitize(r.note) << "\n";
  }
  // Self-integrity trailer: detects a torn or truncated write on load.
  std::string body = os.str();
  os << "checksum " << Sha256::hex_of(body) << "\n";
  return os.str();
}

bool parse_rational(const std::string& tok, Rational& out) {
  auto slash = tok.find('/');
  if (slash == std::string::npos) return false;
  try {
    out = make_rational(std::stoll(tok.substr(0, slash)),
                        std::stoll(tok.substr(slash + 1)));
  } catch (...) {
    return false;
  }
  return true;
}

bool deserialize(const std::string& text, Checkpoint& out, std::string& err) {
  std::istringstream is(text);
  std::string line;
  if (!std::getline(is, line) || line != kMagic) {
    err = "bad magic";
    return false;
  }
  std::string body = line + "\n";
  bool have_checksum = false;
  std::string expected_checksum;
  while (std::getline(is, line)) {
    if (line.rfind("checksum ", 0) == 0) {
      have_checksum = true;
      expected_checksum = line.substr(9);
      break;
    }
    body += line + "\n";
    std::istringstream ls(line);
    std::string key;
    ls >> key;
    try {
      if (key == "input_sha256") ls >> out.input_sha256;
      else if (key == "analysis_version") ls >> out.analysis_version;
      else if (key == "cursor") ls >> out.cursor;
      else if (key == "duplicate_packets") ls >> out.duplicate_packets;
      else if (key == "reordered_packets") ls >> out.reordered_packets;
      else if (key == "lost_packets") ls >> out.lost_packets;
      else if (key == "burst_losses") ls >> out.burst_losses;
      else if (key == "range") {
        DamageRange r;
        std::string a, b, kind, sev;
        ls >> a >> b >> kind >> sev;
        if (!parse_rational(a, r.range.start_ms) ||
            !parse_rational(b, r.range.end_ms)) {
          err = "bad range rational";
          return false;
        }
        auto k = damage_kind_from_string(kind);
        auto s = severity_from_string(sev);
        if (!k || !s) {
          err = "bad range kind/severity";
          return false;
        }
        r.kind = *k;
        r.severity = *s;
        ls >> r.note;
        out.completed_ranges.push_back(std::move(r));
      } else {
        err = "unknown key: " + key;
        return false;
      }
    } catch (...) {
      err = "parse failure at key: " + key;
      return false;
    }
    if (!ls && !ls.eof()) {
      err = "truncated value at key: " + key;
      return false;
    }
  }
  if (!have_checksum) {
    err = "missing checksum trailer";
    return false;
  }
  if (Sha256::hex_of(body) != expected_checksum) {
    err = "checksum mismatch";
    return false;
  }
  return true;
}
}  // namespace

bool save_checkpoint(const std::string& path, const Checkpoint& ckpt) {
  std::string tmp = path + ".tmp";
  std::string text = serialize(ckpt);
  int fd = ::open(tmp.c_str(), O_WRONLY | O_CREAT | O_TRUNC, 0644);
  if (fd < 0) return false;
  const char* p = text.data();
  size_t left = text.size();
  while (left > 0) {
    ssize_t n = ::write(fd, p, left);
    if (n <= 0) {
      ::close(fd);
      ::unlink(tmp.c_str());
      return false;
    }
    p += n;
    left -= static_cast<size_t>(n);
  }
  if (::fsync(fd) != 0) {
    ::close(fd);
    ::unlink(tmp.c_str());
    return false;
  }
  ::close(fd);
  if (::rename(tmp.c_str(), path.c_str()) != 0) {
    ::unlink(tmp.c_str());
    return false;
  }
  return true;
}

LoadResult load_checkpoint(const std::string& path,
                           const std::string& expected_input_hash,
                           uint32_t expected_version) {
  LoadResult res;
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    res.status = LoadStatus::NotFound;
    res.detail = "no checkpoint at " + path;
    return res;
  }
  std::ostringstream ss;
  ss << in.rdbuf();
  std::string err;
  if (!deserialize(ss.str(), res.checkpoint, err)) {
    res.status = LoadStatus::Corrupt;
    res.detail = err;
    return res;
  }
  if (res.checkpoint.input_sha256 != expected_input_hash) {
    res.status = LoadStatus::InputMismatch;
    res.detail = "input changed since checkpoint; refusing to resume";
    return res;
  }
  if (res.checkpoint.analysis_version != expected_version) {
    res.status = LoadStatus::VersionMismatch;
    res.detail = "analysis version changed since checkpoint; refusing to resume";
    return res;
  }
  res.status = LoadStatus::Ok;
  return res;
}

}  // namespace relay
