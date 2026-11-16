#include "image/FrameIO.h"
#include "Errors.h"
#include <algorithm>
#include <cmath>
#include <cstdint>
#include <cstring>
#include <fstream>
#include <sstream>

namespace lg {
namespace {

std::string lowerExt(const std::string& path) {
  auto dot = path.find_last_of('.');
  if (dot == std::string::npos)
    return "";
  std::string e = path.substr(dot + 1);
  std::transform(e.begin(), e.end(), e.begin(),
                 [](unsigned char c) { return std::tolower(c); });
  return e;
}

std::string nextToken(std::istream& in) {
  std::string t;
  while (in >> t) {
    if (!t.empty() && t[0] == '#') {
      std::string rest;
      std::getline(in, rest);
      continue;
    }
    return t;
  }
  throw IoError("unexpected end of image header");
}

int nextInt(std::istream& in) {
  std::string t = nextToken(in);
  try {
    return std::stoi(t);
  } catch (...) {
    throw IoError("bad integer in image header: " + t);
  }
}

void checkSize(int w, int h) {
  if (w <= 0 || h <= 0 || static_cast<long long>(w) * h > 100000000LL)
    throw IoError("implausible frame dimensions " + std::to_string(w) + "x" +
                  std::to_string(h));
}

Frame readPpm(std::istream& in, bool ascii) {
  int w = nextInt(in), h = nextInt(in), maxval = nextInt(in);
  checkSize(w, h);
  if (maxval <= 0 || maxval > 65535)
    throw IoError("unsupported PPM maxval " + std::to_string(maxval));
  Frame f;
  f.w = w;
  f.h = h;
  f.px.resize(static_cast<std::size_t>(w) * h * 3);
  if (ascii) {
    for (auto& v : f.px)
      v = static_cast<float>(nextInt(in)) / maxval;
    return f;
  }
  in.get(); // single whitespace after maxval
  if (maxval < 256) {
    std::vector<char> buf(f.px.size());
    in.read(buf.data(), static_cast<std::streamsize>(buf.size()));
    if (!in)
      throw IoError("truncated PPM raster");
    for (std::size_t i = 0; i < buf.size(); ++i)
      f.px[i] = static_cast<unsigned char>(buf[i]) / 255.0f;
  } else {
    std::vector<unsigned char> buf(f.px.size() * 2);
    in.read(reinterpret_cast<char*>(buf.data()),
            static_cast<std::streamsize>(buf.size()));
    if (!in)
      throw IoError("truncated PPM raster");
    for (std::size_t i = 0; i < f.px.size(); ++i) {
      unsigned v = (buf[2 * i] << 8) | buf[2 * i + 1];
      f.px[i] = v / 65535.0f;
    }
  }
  return f;
}

Frame readPam(std::istream& in) {
  int w = 0, h = 0, depth = 0, maxval = 0;
  std::string line;
  while (std::getline(in, line)) {
    if (!line.empty() && line.back() == '\r')
      line.pop_back();
    if (line == "ENDHDR")
      break;
    std::istringstream ss(line);
    std::string key;
    ss >> key;
    if (key == "WIDTH") ss >> w;
    else if (key == "HEIGHT") ss >> h;
    else if (key == "DEPTH") ss >> depth;
    else if (key == "MAXVAL") ss >> maxval;
  }
  checkSize(w, h);
  if (depth != 3)
    throw IoError("only RGB PAM (DEPTH 3) supported");
  if (maxval <= 0 || maxval > 65535)
    throw IoError("unsupported PAM maxval " + std::to_string(maxval));
  Frame f;
  f.w = w;
  f.h = h;
  f.px.resize(static_cast<std::size_t>(w) * h * 3);
  bool twoBytes = maxval > 255;
  std::vector<unsigned char> buf(f.px.size() * (twoBytes ? 2 : 1));
  in.read(reinterpret_cast<char*>(buf.data()),
          static_cast<std::streamsize>(buf.size()));
  if (!in)
    throw IoError("truncated PAM raster");
  for (std::size_t i = 0; i < f.px.size(); ++i) {
    unsigned v = twoBytes ? ((buf[2 * i] << 8) | buf[2 * i + 1]) : buf[i];
    f.px[i] = static_cast<float>(v) / maxval;
  }
  return f;
}

Frame readFloatFrame(std::istream& in) {
  int w = 0, h = 0;
  in >> w >> h;
  in.get();
  checkSize(w, h);
  Frame f;
  f.w = w;
  f.h = h;
  f.px.resize(static_cast<std::size_t>(w) * h * 3);
  in.read(reinterpret_cast<char*>(f.px.data()),
          static_cast<std::streamsize>(f.px.size() * sizeof(float)));
  if (!in)
    throw IoError("truncated float frame");
  for (float v : f.px)
    if (std::isnan(v))
      throw InternalError("NaN pixel in float frame");
  return f;
}

} // namespace

Frame readFrame(const std::string& path) {
  std::ifstream in(path, std::ios::binary);
  if (!in)
    throw IoError("cannot open frame: " + path);
  char magic[2] = {0, 0};
  in.read(magic, 2);
  if (!in)
    throw IoError("empty frame file: " + path);
  if (magic[0] == 'P' && magic[1] == '6')
    return readPpm(in, false);
  if (magic[0] == 'P' && magic[1] == '3')
    return readPpm(in, true);
  if (magic[0] == 'P' && magic[1] == '7')
    return readPam(in);
  if (magic[0] == 'F' && magic[1] == 'F')
    return readFloatFrame(in);
  throw IoError("unrecognized frame format: " + path);
}

void writeFrame(const Frame& f, const std::string& path) {
  std::string ext = lowerExt(path);
  std::ofstream out(path, std::ios::binary);
  if (!out)
    throw IoError("cannot write frame: " + path);
  auto clamp01 = [](float v) { return v < 0 ? 0.0f : (v > 1 ? 1.0f : v); };
  if (ext == "ppm") {
    out << "P6\n" << f.w << " " << f.h << "\n255\n";
    std::vector<char> buf(f.px.size());
    for (std::size_t i = 0; i < buf.size(); ++i)
      buf[i] = static_cast<char>(static_cast<unsigned char>(
          std::lround(clamp01(f.px[i]) * 255.0f)));
    out.write(buf.data(), static_cast<std::streamsize>(buf.size()));
  } else if (ext == "pam") {
    out << "P7\nWIDTH " << f.w << "\nHEIGHT " << f.h
        << "\nDEPTH 3\nMAXVAL 65535\nTUPLTYPE RGB\nENDHDR\n";
    std::vector<unsigned char> buf(f.px.size() * 2);
    for (std::size_t i = 0; i < f.px.size(); ++i) {
      unsigned v = static_cast<unsigned>(
          std::lround(clamp01(f.px[i]) * 65535.0f));
      buf[2 * i] = static_cast<unsigned char>(v >> 8);
      buf[2 * i + 1] = static_cast<unsigned char>(v & 0xFF);
    }
    out.write(reinterpret_cast<char*>(buf.data()),
              static_cast<std::streamsize>(buf.size()));
  } else if (ext == "ff") {
    out << "FF\n" << f.w << " " << f.h << "\n";
    out.write(reinterpret_cast<const char*>(f.px.data()),
              static_cast<std::streamsize>(f.px.size() * sizeof(float)));
  } else {
    throw IoError("unknown output frame extension: " + path);
  }
  if (!out)
    throw IoError("failed writing frame: " + path);
}

bool isFrameFile(const std::string& path) {
  std::string e = lowerExt(path);
  return e == "ppm" || e == "pam" || e == "ff";
}

Frame makeGradient(int w, int h, double phase) {
  Frame f;
  f.w = w;
  f.h = h;
  f.px.resize(static_cast<std::size_t>(w) * h * 3);
  for (int y = 0; y < h; ++y)
    for (int x = 0; x < w; ++x) {
      std::size_t i = (static_cast<std::size_t>(y) * w + x) * 3;
      double u = w > 1 ? static_cast<double>(x) / (w - 1) : 0.0;
      double v = h > 1 ? static_cast<double>(y) / (h - 1) : 0.0;
      f.px[i] = static_cast<float>(u);
      f.px[i + 1] = static_cast<float>(v);
      f.px[i + 2] = static_cast<float>(
          0.5 + 0.5 * std::sin(phase + 6.2831853 * (u * 0.7 + v * 0.3)));
    }
  return f;
}

} // namespace lg
