// util.hpp — 小端字节 IO、原子写文件、JSON 转义等通用工具
#pragma once
#include "common.hpp"
#include <cstdint>
#include <string>
#include <vector>
#include <ostream>

namespace md {

// ---- 小端编解码（采集流固定小端，与宿主机字节序无关）----
void put_u16(std::vector<uint8_t>& out, uint16_t v);
void put_u32(std::vector<uint8_t>& out, uint32_t v);
void put_u64(std::vector<uint8_t>& out, uint64_t v);
uint16_t get_u16(const uint8_t* p);
uint32_t get_u32(const uint8_t* p);
uint64_t get_u64(const uint8_t* p);

// 回绕感知的无符号差值：a - b，按模数 mod 解释（mod 必须为 2 的幂）
uint64_t wrap_delta(uint64_t a, uint64_t b, uint64_t mod);
int64_t  wrap_signed_delta(uint64_t a, uint64_t b, uint64_t mod);

std::string json_escape(const std::string& s);

// 写临时文件后 rename 原子提交；失败返回 false 并填充 err
bool atomic_write(const std::string& final_path,
                  const std::vector<uint8_t>& data,
                  std::string& err);
bool atomic_append(const std::string& path,
                   const std::vector<uint8_t>& data,
                   std::string& err);

// 输出最终交付 JSON
void write_result_json(std::ostream& os, const PipelineResult& r,
                       const ProcessConfig& cfg);

} // namespace md
