#include "resume_store.hpp"

#include <fstream>
#include <sstream>
#include <utility>

#include "sha256.hpp"
#include "util.hpp"

namespace mediaqc {
namespace {

std::string key_for(const std::string& input_path) {
  // Hash the absolute path so checkpoints from different directories never
  // collide, and so a renamed input cannot silently resume another's output.
  std::filesystem::path p(input_path);
  std::error_code ec;
  auto abs = std::filesystem::absolute(p, ec);
  const std::string canonical = ec ? input_path : abs.lexically_normal().string();
  Sha256 h;
  h.update(canonical);
  return h.hex_digest().substr(0, 32);
}

std::vector<std::string> split(const std::string& s, char sep) {
  std::vector<std::string> out;
  std::string cur;
  for (char c : s) {
    if (c == sep) {
      out.push_back(cur);
      cur.clear();
    } else {
      cur += c;
    }
  }
  out.push_back(cur);
  return out;
}

}  // namespace

const char* job_stage_name(JobStage s) {
  switch (s) {
    case JobStage::Started: return "started";
    case JobStage::Scanned: return "scanned";
    case JobStage::Streaming: return "streaming";
    case JobStage::Written: return "written";
    case JobStage::Verified: return "verified";
    case JobStage::Failed: return "failed";
  }
  return "unknown";
}

bool job_stage_from_name(const std::string& name, JobStage& out) {
  for (JobStage s : {JobStage::Started, JobStage::Scanned, JobStage::Streaming,
                     JobStage::Written, JobStage::Verified, JobStage::Failed}) {
    if (name == job_stage_name(s)) {
      out = s;
      return true;
    }
  }
  return false;
}

std::string config_digest(const std::string& config_summary) {
  Sha256 h;
  h.update("mediaqc-config-v1\n");
  h.update(config_summary);
  return h.hex_digest();
}

ResumeStore::ResumeStore(std::filesystem::path dir) : dir_(std::move(dir)) {}

std::filesystem::path ResumeStore::checkpoint_path(const std::string& input_path) const {
  return dir_ / (key_for(input_path) + ".ckpt");
}

std::string serialize_checkpoint(const Checkpoint& cp) {
  std::ostringstream os;
  os << "version=" << cp.version << "\n";
  os << "stage=" << job_stage_name(cp.stage) << "\n";
  os << "input_path=" << json_escape(cp.input_path) << "\n";
  os << "input_sha256=" << cp.input_sha256 << "\n";
  os << "input_size=" << cp.input_size << "\n";
  os << "config_digest=" << cp.config_digest << "\n";
  os << "output_path=" << json_escape(cp.output_path) << "\n";
  os << "input_offset=" << cp.input_offset << "\n";
  os << "packets_written=" << cp.packets_written << "\n";
  os << "bytes_written=" << cp.bytes_written << "\n";
  os << "repairs=" << cp.repairs << "\n";
  os << "errors=" << cp.errors << "\n";
  os << "dropped=" << cp.dropped << "\n";
  os << "output_sha256=" << cp.output_sha256 << "\n";
  os << "written_at=" << cp.written_at << "\n";
  os << "streams=" << cp.stream_indices.size() << "\n";
  for (size_t i = 0; i < cp.stream_indices.size(); ++i) {
    const TimelineState& s = cp.per_stream[i];
    os << "stream=" << cp.stream_indices[i] << "," << s.last_out_dts << ","
       << s.last_raw_dts << "," << s.last_pts << "," << s.last_duration << ","
       << s.wrap_offset << "," << s.shift << "," << s.packets << "\n";
  }
  return os.str();
}

bool parse_checkpoint(const std::string& text, Checkpoint& cp, std::string& err) {
  cp = Checkpoint{};
  std::istringstream in(text);
  std::string line;
  bool saw_sha = false;
  bool saw_cfg = false;
  while (std::getline(in, line)) {
    if (line.empty()) continue;
    const auto eq = line.find('=');
    if (eq == std::string::npos) {
      err = "malformed checkpoint line: " + line;
      return false;
    }
    const std::string k = line.substr(0, eq);
    const std::string v = line.substr(eq + 1);
    try {
      if (k == "version") cp.version = std::stoi(v);
      else if (k == "stage") {
        if (!job_stage_from_name(v, cp.stage)) {
          err = "unknown checkpoint stage: " + v;
          return false;
        }
      } else if (k == "input_path") cp.input_path = v;
      else if (k == "input_sha256") { cp.input_sha256 = v; saw_sha = !v.empty(); }
      else if (k == "input_size") cp.input_size = std::stoll(v);
      else if (k == "config_digest") { cp.config_digest = v; saw_cfg = !v.empty(); }
      else if (k == "output_path") cp.output_path = v;
      else if (k == "input_offset") cp.input_offset = std::stoll(v);
      else if (k == "packets_written") cp.packets_written = std::stoll(v);
      else if (k == "bytes_written") cp.bytes_written = std::stoll(v);
      else if (k == "repairs") cp.repairs = std::stoll(v);
      else if (k == "errors") cp.errors = std::stoll(v);
      else if (k == "dropped") cp.dropped = std::stoll(v);
      else if (k == "output_sha256") cp.output_sha256 = v;
      else if (k == "written_at") cp.written_at = v;
      else if (k == "streams") { /* count only; stream lines follow */ }
      else if (k == "stream") {
        const auto f = split(v, ',');
        if (f.size() != 8) {
          err = "malformed stream state: " + v;
          return false;
        }
        TimelineState s;
        cp.stream_indices.push_back(std::stoi(f[0]));
        s.last_out_dts = std::stoll(f[1]);
        s.last_raw_dts = std::stoll(f[2]);
        s.last_pts = std::stoll(f[3]);
        s.last_duration = std::stoll(f[4]);
        s.wrap_offset = std::stoll(f[5]);
        s.shift = std::stoll(f[6]);
        s.packets = std::stoll(f[7]);
        cp.per_stream.push_back(s);
      }
      // Unknown keys are ignored so newer checkpoints stay readable.
    } catch (const std::exception&) {
      err = "checkpoint value is not a valid number for key " + k;
      return false;
    }
  }
  if (!saw_sha || !saw_cfg) {
    err = "checkpoint is missing its input checksum or config digest";
    return false;
  }
  if (cp.input_sha256.size() != 64 || cp.config_digest.size() != 64) {
    err = "checkpoint digest has the wrong width";
    return false;
  }
  if (cp.per_stream.size() != cp.stream_indices.size()) {
    err = "checkpoint stream table is inconsistent";
    return false;
  }
  return true;
}

bool ResumeStore::save(const Checkpoint& cp, std::string& err) {
  std::error_code ec;
  std::filesystem::create_directories(dir_, ec);
  if (ec) {
    err = "cannot create checkpoint directory " + dir_.string() + ": " + ec.message();
    return false;
  }
  const auto path = checkpoint_path(cp.input_path);
  return write_file_atomic(path, serialize_checkpoint(cp), err);
}

const char* checkpoint_refusal_code(Code c) { return code_name(c); }

bool ResumeStore::load_for(const std::string& input_path, const std::string& input_sha256,
                           const std::string& cfg_digest, const std::string& output_path,
                           ResumeDecision& decision, Error& err) {
  decision = ResumeDecision{};
  err = Error{};
  err.file = input_path;
  err.stage = Stage::Write;
  err.severity = Severity::Recovered;
  const auto path = checkpoint_path(input_path);
  std::error_code ec;
  if (!std::filesystem::exists(path, ec)) {
    decision.reason = "no checkpoint for this input";
    return true;
  }
  std::string text;
  if (!read_file(path, text, 1 << 20)) {
    decision.reason = "checkpoint file is unreadable or oversized";
    err.code = Code::CheckpointCorrupt;
    err.detail = decision.reason + ": " + path.string();
    err.action = "start from the beginning";
    return true;
  }
  Checkpoint cp;
  std::string perr;
  if (!parse_checkpoint(text, cp, perr)) {
    decision.reason = "checkpoint failed validation: " + perr;
    err.code = Code::CheckpointCorrupt;
    err.detail = decision.reason;
    err.action = "discard the checkpoint and start from the beginning";
    return true;
  }
  decision.checkpoint = cp;
  auto refuse = [&](Code code, const std::string& why) {
    decision.resume = false;
    decision.reason = why;
    err.code = code;
    err.detail = why;
    err.action = "run without --resume for this asset";
    return true;
  };
  if (cp.input_sha256 != input_sha256) {
    return refuse(Code::CheckpointMismatch,
                  "input checksum changed since the checkpoint (" +
                      cp.input_sha256.substr(0, 12) + " -> " +
                      input_sha256.substr(0, 12) + ")");
  }
  if (cp.config_digest != cfg_digest) {
    return refuse(Code::CheckpointMismatch,
                  "configuration changed since the checkpoint");
  }
  if (!output_path.empty() && !cp.output_path.empty() && cp.output_path != output_path) {
    return refuse(Code::CheckpointMismatch,
                  "checkpoint belongs to output " + cp.output_path);
  }
  if (cp.stage == JobStage::Failed) {
    return refuse(Code::CheckpointMismatch, "previous run already marked this asset failed");
  }
  if (cp.stage != JobStage::Streaming && cp.stage != JobStage::Written &&
      cp.stage != JobStage::Verified) {
    decision.reason = std::string("checkpoint stage '") + job_stage_name(cp.stage) +
                      "' has no committed media to resume";
    return true;
  }
  if (cp.input_size > 0 && cp.input_offset > cp.input_size) {
    return refuse(Code::CheckpointMismatch,
                  "checkpoint offset " + std::to_string(cp.input_offset) +
                      " exceeds input size " + std::to_string(cp.input_size));
  }
  // Estimate time saved from the fraction of the input already committed.
  const int64_t covered = cp.stage == JobStage::Verified ? cp.input_size : cp.input_offset;
  if (cp.input_size > 0) {
    decision.seconds_saved = covered > 0 ? 1 : 0;
  }
  decision.resume = true;
  decision.reason = "checkpoint matches input, config and output";
  return true;
}

bool ResumeStore::remove(const std::string& input_path) {
  std::error_code ec;
  return std::filesystem::remove(checkpoint_path(input_path), ec);
}

}  // namespace mediaqc
