#include "json_writer.hpp"

#include <cmath>
#include <cstdio>
#include <string>

#include "sha256.hpp"

namespace mfs {
namespace {

std::string escape_json(const std::string& text) {
  std::string out;
  out.reserve(text.size() + 2);
  for (unsigned char c : text) {
    switch (c) {
      case '"':
        out += "\\\"";
        break;
      case '\\':
        out += "\\\\";
        break;
      case '\n':
        out += "\\n";
        break;
      case '\r':
        out += "\\r";
        break;
      case '\t':
        out += "\\t";
        break;
      default:
        if (c < 0x20) {
          char buffer[8];
          std::snprintf(buffer, sizeof(buffer), "\\u%04x", c);
          out += buffer;
        } else {
          out.push_back(static_cast<char>(c));
        }
    }
  }
  return out;
}

std::string format_double(double value) {
  if (!std::isfinite(value)) {
    return "0";
  }
  char buffer[32];
  std::snprintf(buffer, sizeof(buffer), "%.6f", value);
  return buffer;
}

}  // namespace

void JsonWriter::indent() {
  out_.append(static_cast<size_t>(frames_.size() - 1) * 2, ' ');
}

void JsonWriter::begin_item() {
  if (frames_.empty()) {
    return;  // value at document root: nothing to separate
  }
  char& first = frames_.back();
  if (first == 0) {
    out_.push_back(',');
  }
  first = 0;
  if (!out_.empty()) {
    out_.push_back('\n');
    indent();
  }
}

void JsonWriter::key_prefix(const std::string& key) {
  out_.push_back('"');
  out_ += escape_json(key);
  out_ += "\": ";
}

void JsonWriter::open_container(const char* opener) {
  out_ += opener;
  frames_.push_back(1);
}

void JsonWriter::close_container(char closer) {
  const bool empty = frames_.back() != 0;
  frames_.pop_back();
  if (!empty) {
    out_.push_back('\n');
    indent();
  }
  out_.push_back(closer);
}

void JsonWriter::begin_object() {
  begin_item();
  open_container("{");
}

void JsonWriter::begin_object(const std::string& key) {
  begin_item();
  key_prefix(key);
  open_container("{");
}

void JsonWriter::end_object() { close_container('}'); }

void JsonWriter::begin_array(const std::string& key) {
  begin_item();
  key_prefix(key);
  open_container("[");
}

void JsonWriter::end_array() { close_container(']'); }

void JsonWriter::field(const std::string& key, const std::string& value) {
  begin_item();
  key_prefix(key);
  out_.push_back('"');
  out_ += escape_json(value);
  out_.push_back('"');
}

void JsonWriter::field(const std::string& key, const char* value) {
  field(key, std::string(value));
}

void JsonWriter::field(const std::string& key, int64_t value) {
  begin_item();
  key_prefix(key);
  out_ += std::to_string(value);
}

void JsonWriter::field(const std::string& key, uint64_t value) {
  begin_item();
  key_prefix(key);
  out_ += std::to_string(value);
}

void JsonWriter::field(const std::string& key, uint32_t value) {
  begin_item();
  key_prefix(key);
  out_ += std::to_string(value);
}

void JsonWriter::field(const std::string& key, double value) {
  begin_item();
  key_prefix(key);
  out_ += format_double(value);
}

void JsonWriter::field(const std::string& key, bool value) {
  begin_item();
  key_prefix(key);
  out_ += value ? "true" : "false";
}

void JsonWriter::hex_field(const std::string& key,
                           const Sha256Digest& digest) {
  field(key, Sha256::hex(digest));
}

}  // namespace mfs
