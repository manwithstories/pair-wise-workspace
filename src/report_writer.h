#pragma once
// 报告输出：终端表格与机器可读 JSON。JSON 字段名固定（snake_case），
// 数值字段稳定，便于自动化比对；不引入第三方 JSON 库。
#include <ostream>
#include <string>

#include "gop_analyzer.h"

namespace h264 {

enum class ReportFormat { TABLE, JSON };

class ReportWriter {
public:
  // 把分析结果写入 out。mode_strict 影响头部模式标注，不改变数据。
  static void write(std::ostream &out, const AnalysisResult &r,
                    ReportFormat fmt, bool mode_strict,
                    const std::string &source_name);

  static std::string jsonEscape(const std::string &s);
};

} // namespace h264
