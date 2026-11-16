#include "lut/LutParser.h"
#include "Errors.h"
#include <algorithm>
#include <array>
#include <cctype>
#include <cerrno>
#include <cmath>
#include <cstdlib>
#include <fstream>
#include <sstream>

namespace lg {
namespace {

bool isSupportedGrid(int n) { return n == 17 || n == 32 || n == 33 || n == 65; }

std::vector<std::string> readLines(const std::string& path) {
  std::ifstream in(path);
  if (!in)
    throw IoError("cannot open LUT file: " + path);
  std::vector<std::string> out;
  std::string line;
  while (std::getline(in, line)) {
    if (!line.empty() && line.back() == '\r')
      line.pop_back();
    out.push_back(line);
  }
  return out;
}

std::vector<std::string> tokenize(const std::string& line) {
  std::istringstream ss(line);
  std::vector<std::string> tok;
  std::string t;
  while (ss >> t)
    tok.push_back(t);
  return tok;
}

double parseNumber(const std::string& tok, int ln) {
  char* end = nullptr;
  errno = 0;
  double v = std::strtod(tok.c_str(), &end);
  if (end == tok.c_str() || *end != '\0' || errno == ERANGE)
    throw LutError("line " + std::to_string(ln) + ": invalid number '" + tok + "'");
  return v;
}

bool isNumberToken(const std::string& tok) {
  if (tok.empty())
    return false;
  char* end = nullptr;
  std::strtod(tok.c_str(), &end);
  return end != tok.c_str() && *end == '\0';
}

std::string lowerExt(const std::string& path) {
  auto dot = path.find_last_of('.');
  if (dot == std::string::npos)
    return "";
  std::string e = path.substr(dot + 1);
  std::transform(e.begin(), e.end(), e.begin(),
                 [](unsigned char c) { return std::tolower(c); });
  return e;
}

void checkGrid(int n, int ln) {
  if (!isSupportedGrid(n))
    throw LutError("line " + std::to_string(ln) + ": unsupported grid size " +
                   std::to_string(n) + " (expected 17, 32, 33 or 65)");
}

void checkDomain(const LutTable& t, int ln) {
  for (int c = 0; c < 3; ++c)
    if (!(t.dmax[c] > t.dmin[c]))
      throw LutError("line " + std::to_string(ln) +
                     ": zero-width domain on channel " + std::to_string(c));
}

void expectCount(std::size_t got, std::size_t expected, int ln) {
  if (got != expected)
    throw LutError("line " + std::to_string(ln) + ": expected " +
                   std::to_string(expected) + " entries, got " +
                   std::to_string(got));
}

LutInfo parseCube(const std::vector<std::string>& lines) {
  LutInfo info;
  info.format = "cube";
  info.order = "r-fastest";
  int n = -1;
  std::vector<std::array<double, 3>> vals;
  for (std::size_t i = 0; i < lines.size(); ++i) {
    int ln = static_cast<int>(i) + 1;
    auto tok = tokenize(lines[i]);
    if (tok.empty() || tok[0][0] == '#')
      continue;
    const std::string& k = tok[0];
    if (k == "LUT_3D_SIZE") {
      if (tok.size() < 2)
        throw LutError("line " + std::to_string(ln) + ": LUT_3D_SIZE needs a value");
      n = static_cast<int>(parseNumber(tok[1], ln));
      checkGrid(n, ln);
    } else if (k == "DOMAIN_MIN" || k == "DOMAIN_MAX") {
      if (tok.size() < 4)
        throw LutError("line " + std::to_string(ln) + ": " + k + " needs 3 values");
      double* dst = (k == "DOMAIN_MIN") ? info.table.dmin : info.table.dmax;
      for (int c = 0; c < 3; ++c)
        dst[c] = parseNumber(tok[1 + c], ln);
    } else if (k == "LUT_1D_SIZE") {
      throw LutError("line " + std::to_string(ln) + ": 1D shaper LUTs not supported");
    } else if (k == "TITLE" || k == "COMMENT") {
      continue;
    } else if (isNumberToken(k)) {
      if (tok.size() < 3)
        throw LutError("line " + std::to_string(ln) + ": expected 3 components");
      vals.push_back({parseNumber(tok[0], ln), parseNumber(tok[1], ln),
                      parseNumber(tok[2], ln)});
      if (n > 0 && vals.size() > static_cast<std::size_t>(n) * n * n)
        expectCount(vals.size(), static_cast<std::size_t>(n) * n * n, ln);
    } else {
      throw LutError("line " + std::to_string(ln) + ": unrecognized keyword '" + k + "'");
    }
  }
  if (n < 0)
    throw LutError("missing LUT_3D_SIZE declaration");
  std::size_t expected = static_cast<std::size_t>(n) * n * n;
  expectCount(vals.size(), expected, static_cast<int>(lines.size()));
  info.table.size = n;
  info.table.data.reserve(expected * 3);
  for (auto& v : vals)
    for (int c = 0; c < 3; ++c)
      info.table.data.push_back(static_cast<float>(v[c]));
  checkDomain(info.table, static_cast<int>(lines.size()));
  return info;
}

LutInfo parse3dl(const std::vector<std::string>& lines) {
  LutInfo info;
  info.format = "3dl";
  info.order = "b-fastest";
  int n = 0;
  std::vector<std::array<double, 3>> rows;
  for (std::size_t i = 0; i < lines.size(); ++i) {
    int ln = static_cast<int>(i) + 1;
    auto tok = tokenize(lines[i]);
    if (tok.empty() || tok[0][0] == '#')
      continue;
    if (tok[0] == "3DMESH")
      continue;
    if (tok[0] == "Mesh") {
      if (tok.size() >= 3 && static_cast<int>(parseNumber(tok[2], ln)) != 12)
        throw LutError("line " + std::to_string(ln) + ": only 12-bit .3dl supported");
      continue;
    }
    if (!isNumberToken(tok[0]))
      throw LutError("line " + std::to_string(ln) + ": unrecognized token '" + tok[0] + "'");
    if (n == 0) { // shaper line: its value count defines the grid
      n = static_cast<int>(tok.size());
      checkGrid(n, ln);
      continue;
    }
    if (tok.size() < 3)
      throw LutError("line " + std::to_string(ln) + ": expected 3 components");
    std::array<double, 3> v;
    for (int c = 0; c < 3; ++c) {
      v[c] = parseNumber(tok[c], ln);
      if (v[c] < 0.0 || v[c] > 4095.0 || v[c] != std::floor(v[c]))
        throw LutError("line " + std::to_string(ln) +
                       ": expected 12-bit integer in [0,4095]");
      v[c] /= 4095.0;
    }
    rows.push_back(v);
  }
  if (n == 0)
    throw LutError("missing shaper line in .3dl file");
  std::size_t expected = static_cast<std::size_t>(n) * n * n;
  expectCount(rows.size(), expected, static_cast<int>(lines.size()));
  info.table.size = n;
  info.table.data.resize(expected * 3);
  for (std::size_t k = 0; k < expected; ++k) { // file order: b fastest
    int r = static_cast<int>(k / (n * n));
    int g = static_cast<int>((k / n) % n);
    int b = static_cast<int>(k % n);
    float* dst = info.table.node(r, g, b);
    for (int c = 0; c < 3; ++c)
      dst[c] = static_cast<float>(rows[k][c]);
  }
  return info;
}

LutInfo parseSpi3d(const std::vector<std::string>& lines) {
  LutInfo info;
  info.format = "spi3d";
  info.order = "indexed";
  int n = 0;
  int state = 0; // 0: expect SPILUT, 1: expect "3 3", 2: expect dims, 3: data
  std::size_t filled = 0, expected = 0;
  for (std::size_t i = 0; i < lines.size(); ++i) {
    int ln = static_cast<int>(i) + 1;
    auto tok = tokenize(lines[i]);
    if (tok.empty() || tok[0][0] == '#')
      continue;
    if (state == 0) {
      if (tok[0] != "SPILUT")
        throw LutError("line " + std::to_string(ln) + ": expected 'SPILUT 1.0'");
      state = 1;
    } else if (state == 1) {
      if (tok.size() < 2 || parseNumber(tok[0], ln) != 3 || parseNumber(tok[1], ln) != 3)
        throw LutError("line " + std::to_string(ln) + ": expected '3 3'");
      state = 2;
    } else if (state == 2) {
      if (tok.size() < 3)
        throw LutError("line " + std::to_string(ln) + ": expected grid dimensions");
      int a = static_cast<int>(parseNumber(tok[0], ln));
      int b = static_cast<int>(parseNumber(tok[1], ln));
      int c = static_cast<int>(parseNumber(tok[2], ln));
      if (a != b || b != c)
        throw LutError("line " + std::to_string(ln) + ": non-cubic grid not supported");
      checkGrid(a, ln);
      n = a;
      expected = static_cast<std::size_t>(n) * n * n;
      info.table.size = n;
      info.table.data.resize(expected * 3);
      state = 3;
    } else {
      if (tok.size() < 6)
        throw LutError("line " + std::to_string(ln) + ": expected 'i j k r g b'");
      int idx[3];
      for (int c = 0; c < 3; ++c) {
        idx[c] = static_cast<int>(parseNumber(tok[c], ln));
        if (idx[c] < 0 || idx[c] >= n)
          throw LutError("line " + std::to_string(ln) + ": index out of range");
      }
      float* dst = info.table.node(idx[0], idx[1], idx[2]);
      for (int c = 0; c < 3; ++c)
        dst[c] = static_cast<float>(parseNumber(tok[3 + c], ln));
      ++filled;
    }
  }
  if (state != 3)
    throw LutError("truncated .spi3d header");
  expectCount(filled, expected, static_cast<int>(lines.size()));
  return info;
}

} // namespace

LutInfo parseLut(const std::string& path) {
  std::string ext = lowerExt(path);
  auto lines = readLines(path);
  if (ext == "cube")
    return parseCube(lines);
  if (ext == "3dl")
    return parse3dl(lines);
  if (ext == "spi3d")
    return parseSpi3d(lines);
  for (const auto& l : lines) { // sniff content when extension is unknown
    auto tok = tokenize(l);
    if (tok.empty() || tok[0][0] == '#')
      continue;
    if (tok[0] == "SPILUT")
      return parseSpi3d(lines);
    if (tok[0] == "LUT_3D_SIZE" || tok[0] == "TITLE" || tok[0] == "DOMAIN_MIN")
      return parseCube(lines);
    return parse3dl(lines);
  }
  throw LutError("empty LUT file: " + path);
}

} // namespace lg
