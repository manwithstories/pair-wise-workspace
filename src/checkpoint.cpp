#include "relay/checkpoint.hpp"

#include <algorithm>
#include <fstream>
#include <sstream>

namespace relay {
namespace {

std::string status_text(StageStatus status) {
  switch (status) {
    case StageStatus::kPending: return "pending";
    case StageStatus::kRunning: return "running";
    case StageStatus::kDone: return "done";
    case StageStatus::kFailed: return "failed";
    case StageStatus::kSkipped: return "skipped";
  }
  return "pending";
}

StageStatus parse_status(const std::string& text) {
  if (text == "running") return StageStatus::kRunning;
  if (text == "done") return StageStatus::kDone;
  if (text == "failed") return StageStatus::kFailed;
  if (text == "skipped") return StageStatus::kSkipped;
  return StageStatus::kPending;
}

std::string escape(const std::string& value) {
  std::string out;
  out.reserve(value.size());
  for (const char c : value) {
    if (c == '\\') {
      out += "\\\\";
    } else if (c == '\n') {
      out += "\\n";
    } else if (c == '\t') {
      out += "\\t";
    } else {
      out.push_back(c);
    }
  }
  return out;
}

std::string unescape(const std::string& value) {
  std::string out;
  for (std::size_t i = 0; i < value.size(); ++i) {
    if (value[i] != '\\' || i + 1 >= value.size()) {
      out.push_back(value[i]);
      continue;
    }
    const char next = value[++i];
    out.push_back(next == 'n' ? '\n' : next == 't' ? '\t' : next);
  }
  return out;
}

}  // namespace

CheckpointStore::CheckpointStore(std::filesystem::path directory)
    : path_(std::move(directory) / "checkpoint.ckpt") {}

bool CheckpointStore::load(ErrorSink& errors) {
  if (loaded_) {
    return true;
  }
  loaded_ = true;
  if (!std::filesystem::exists(path_)) {
    return true;
  }
  std::ifstream in(path_);
  if (!in) {
    load_error_ = "cannot open checkpoint " + path_.string();
    errors.fail("resume", "", 0, load_error_);
    return false;
  }
  std::string magic;
  std::getline(in, magic);
  if (magic != "RELAYCHECK 1") {
    load_error_ = "checkpoint magic rejected";
    errors.fail("resume", "", 0, load_error_);
    return false;
  }
  std::string line;
  while (std::getline(in, line)) {
    if (line.empty()) {
      continue;
    }
    std::istringstream tokens(line);
    std::string kind;
    tokens >> kind;
    if (kind != "STAGE") {
      load_error_ = "unknown checkpoint record: " + kind;
      errors.fail("resume", "", 0, load_error_);
      return false;
    }
    StageRecord record;
    std::string status;
    if (!(tokens >> record.name >> status >> record.input_digest >>
          record.output_digest)) {
      load_error_ = "truncated checkpoint record";
      errors.fail("resume", record.name, 0, load_error_);
      return false;
    }
    record.status = parse_status(status);
    if (record.input_digest == "-") {
      record.input_digest.clear();
    }
    if (record.output_digest == "-") {
      record.output_digest.clear();
    }
    std::string rest;
    std::getline(tokens, rest);
    while (!rest.empty() && rest.front() == ' ') {
      rest.erase(rest.begin());
    }
    if (rest.rfind("payload=", 0) == 0) {
      record.payload = unescape(rest.substr(8));
    } else if (rest.rfind("error=", 0) == 0) {
      record.error = unescape(rest.substr(6));
    } else if (!rest.empty()) {
      load_error_ = "malformed checkpoint payload";
      errors.fail("resume", record.name, 0, load_error_);
      return false;
    }
    if (records_.count(record.name) == 0) {
      order_.push_back(record.name);
    }
    records_[record.name] = std::move(record);
  }
  return true;
}

bool CheckpointStore::flush(ErrorSink& errors) {
  namespace fs = std::filesystem;
  std::error_code ec;
  fs::create_directories(path_.parent_path(), ec);
  if (ec) {
    errors.fail("checkpoint", "", 0,
                "cannot create checkpoint directory: " + ec.message());
    return false;
  }
  const fs::path temporary = path_.string() + ".tmp";
  {
    std::ofstream out(temporary, std::ios::trunc);
    if (!out) {
      errors.fail("checkpoint", "", 0,
                  "cannot write " + temporary.string());
      return false;
    }
    out << "RELAYCHECK 1\n";
    for (const auto& name : order_) {
      auto it = records_.find(name);
      if (it == records_.end()) {
        continue;
      }
      const StageRecord& record = it->second;
      const std::string input = record.input_digest.empty() ? "-" : record.input_digest;
      const std::string output =
          record.output_digest.empty() ? "-" : record.output_digest;
      out << "STAGE " << record.name << ' ' << status_text(record.status) << ' '
          << input << ' ' << output;
      if (!record.payload.empty()) {
        out << " payload=" << escape(record.payload);
      } else if (!record.error.empty()) {
        out << " error=" << escape(record.error);
      }
      out << '\n';
    }
  }
  fs::rename(temporary, path_, ec);
  if (ec) {
    errors.fail("checkpoint", "", 0,
                "cannot replace checkpoint: " + ec.message());
    return false;
  }
  return true;
}

bool CheckpointStore::can_reuse(const std::string& stage,
                                const std::string& input_digest,
                                StageOutcome& outcome,
                                ErrorSink& errors) const {
  auto self = const_cast<CheckpointStore*>(this);
  if (!self->load(errors)) {
    return false;
  }
  auto it = records_.find(stage);
  if (it == records_.end()) {
    return false;
  }
  outcome.record = it->second;
  if (it->second.status != StageStatus::kDone) {
    return false;
  }
  if (it->second.input_digest != input_digest) {
    outcome.rejected = true;
    std::string chain = stage;
    // Walk the completed stages to show which upstream digest changed.
    for (auto order_it = order_.rbegin(); order_it != order_.rend(); ++order_it) {
      if (*order_it == stage) {
        continue;
      }
      chain += "<-" + *order_it;
      if (*order_it == kStageRead) {
        break;
      }
    }
    errors.warn("resume", stage, 0,
                "input digest changed (" + it->second.input_digest.substr(0, 12) +
                    " -> " + input_digest.substr(0, 12) +
                    "); refusing to reuse stale output",
                chain);
    return false;
  }
  outcome.reused = true;
  outcome.satisfied = true;
  return true;
}

void CheckpointStore::begin(const std::string& stage,
                            const std::string& input_digest,
                            ErrorSink& errors) {
  if (!load(errors)) {
    return;
  }
  StageRecord& record = records_[stage];
  if (record.name.empty()) {
    record.name = stage;
    order_.push_back(stage);
  }
  record.input_digest = input_digest;
  record.status = StageStatus::kRunning;
  record.payload.clear();
  record.error.clear();
  flush(errors);
}

bool CheckpointStore::complete(const std::string& stage,
                               const std::string& input_digest,
                               const std::string& output_digest,
                               const std::string& payload,
                               ErrorSink& errors) {
  if (!load(errors)) {
    return false;
  }
  StageRecord& record = records_[stage];
  if (record.name.empty()) {
    record.name = stage;
    order_.push_back(stage);
  }
  record.input_digest = input_digest;
  record.output_digest = output_digest;
  record.payload = payload;
  record.status = StageStatus::kDone;
  record.error.clear();
  return flush(errors);
}

void CheckpointStore::fail(const std::string& stage,
                           const std::string& input_digest,
                           const std::string& reason, ErrorSink& errors) {
  if (!load(errors)) {
    return;
  }
  StageRecord& record = records_[stage];
  if (record.name.empty()) {
    record.name = stage;
    order_.push_back(stage);
  }
  record.input_digest = input_digest;
  record.status = StageStatus::kFailed;
  record.error = reason;
  flush(errors);
}

std::optional<StageRecord> CheckpointStore::find(const std::string& stage) const {
  auto self = const_cast<CheckpointStore*>(this);
  ErrorSink ignored;
  if (!self->load(ignored)) {
    return std::nullopt;
  }
  auto it = records_.find(stage);
  if (it == records_.end()) {
    return std::nullopt;
  }
  return it->second;
}

std::vector<std::string> CheckpointStore::completed_order() const {
  std::vector<std::string> out;
  for (const auto& name : order_) {
    auto it = records_.find(name);
    if (it != records_.end() && it->second.status == StageStatus::kDone) {
      out.push_back(name);
    }
  }
  return out;
}

}  // namespace relay
