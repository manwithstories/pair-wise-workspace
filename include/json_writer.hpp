#pragma once

// Minimal JSON writer: object, array, string, integer, double and bool. The
// writer is stateful and relies on nesting frames to place separators, so it
// serialises any well-formed tree without the caller tracking commas.

#include <cstdint>
#include <string>
#include <vector>

#include "sha256.hpp"

namespace mfs {

class JsonWriter {
 public:
  void begin_object();
  void begin_object(const std::string& key);
  void end_object();
  void begin_array(const std::string& key);
  void end_array();
  void field(const std::string& key, const std::string& value);
  void field(const std::string& key, const char* value);
  void field(const std::string& key, int64_t value);
  void field(const std::string& key, uint64_t value);
  void field(const std::string& key, uint32_t value);
  void field(const std::string& key, double value);
  void field(const std::string& key, bool value);
  void hex_field(const std::string& key, const Sha256Digest& digest);

  const std::string& str() const { return out_; }

 private:
  // Emits the separator for the next item at the current nesting level.
  void begin_item();
  void indent();
  void key_prefix(const std::string& key);
  void open_container(const char* opener);
  void close_container(char closer);

  std::string out_;
  // One entry per open container: 1 while no item has been emitted yet.
  std::vector<char> frames_{1};
};

}  // namespace mfs
