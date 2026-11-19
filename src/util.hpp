#pragma once
// 通用工具：带源位置的错误模型、有界限制常量、SHA-256、JSON 写出、路径约束。
#include <cstdint>
#include <filesystem>
#include <sstream>
#include <string>
#include <vector>

namespace mb {

// 所有模块共用的错误记录：稳定错误码 + 描述 + 源位置（字节偏移或帧号）。
struct ErrRecord {
  std::string code;
  std::string message;
  int64_t source_offset = -1;  // -1 表示不适用
};
using ErrList = std::vector<ErrRecord>;

// 有界限制：超限即有界失败，不允许无限增长。
inline constexpr size_t kMaxTracks = 16;
inline constexpr size_t kMaxSubtitleEntries = 50000;
inline constexpr size_t kMaxSubtitleText = 2048;
inline constexpr size_t kMaxQueueCapacity = 256;

// ---- SHA-256（自实现，无外部依赖）----
std::string sha256_hex(const void* data, size_t len);
inline std::string sha256_hex(const std::string& s) {
  return sha256_hex(s.data(), s.size());
}
// 失败时返回空串并填充 err。
std::string sha256_file(const std::filesystem::path& p, std::string* err);

// ---- 极简 JSON 写出器（对象/数组/字符串/数字/布尔）----
class JsonWriter {
 public:
  JsonWriter() { s_ << '{'; top_ = {false}; open_ = {'{'}; }
  void key(const std::string& k);
  void val(const std::string& v);
  void val(int64_t v);
  void val(uint64_t v) { val(static_cast<int64_t>(v)); }
  void val(double v);
  void val(bool v);
  void begin_obj(const std::string& k);   // "k":{
  void begin_obj();
  void begin_arr(const std::string& k);   // "k":[
  void begin_arr();
  void end();                             // 关闭最近一层 { 或 [
  std::string str() const;                // 自动补齐未关闭层
 private:
  void sep();
  std::ostringstream s_;
  std::vector<bool> top_;  // 每层是否已写过元素
  std::vector<char> open_; // 每层的开括号：'{' 或 '['
};

// ---- 路径与目录 ----
// p 解析（弱规范化）后必须位于 base 之内，防止越出工作目录。
bool path_within(const std::filesystem::path& base,
                 const std::filesystem::path& p);
bool ensure_dir(const std::filesystem::path& p, std::string* err);
uint64_t file_size(const std::filesystem::path& p);

}  // namespace mb
