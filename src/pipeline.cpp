#include "pipeline.hpp"

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <map>
#include <set>

extern "C" {
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/log.h>
}

#include "fixtures.hpp"
#include "index_writer.hpp"
#include "resume_store.hpp"
#include "scanner.hpp"
#include "sha256.hpp"
#include "timeline.hpp"
#include "util.hpp"
#include "verifier.hpp"

namespace mediaqc {
namespace {

constexpr const char* kSidecarSuffix = ".qc.json";

int64_t now_seconds() { return epoch_seconds_now(); }

std::string output_name_for(const std::filesystem::path& input) {
  return input.filename().string();
}

bool is_media_extension(const std::filesystem::path& p) {
  std::string ext = p.extension().string();
  std::transform(ext.begin(), ext.end(), ext.begin(),
                 [](unsigned char c) { return char(std::tolower(c)); });
  return ext == ".mp4" || ext == ".mov" || ext == ".m4v" || ext == ".m4a";
}

// Persists the per-file evidence the verify subcommand needs: the output
// hash, the input hash and the localised errors with their input offsets.
std::string render_sidecar(const FileResult& r) {
  JsonWriter w;
  w.begin_object();
  w.key("input").value(r.input_path);
  w.key("input_sha256").value(r.input_sha256);
  w.key("input_bytes").value(r.input_bytes);
  w.key("output").value(r.output_path);
  w.key("output_sha256").value(r.output_sha256);
  w.key("output_bytes").value(r.output_bytes);
  w.key("journal").value(r.journal_path);
  w.key("status").value(file_status_name(r.status));
  w.key("packets").value(r.packets);
  w.key("index_entries").value(r.index_entries);
  w.key("keyframes").value(r.keyframes);
  w.key("decode_errors").value(r.decode_errors);
  w.key("repairs").value(r.repairs);
  w.key("dropped_packets").value(r.dropped_packets);
  w.key("first_error_offset_bytes").value(r.first_error_offset);
  w.key("offset_error_bytes").value(r.offset_error_bytes);
  w.key("errors").begin_array();
  for (const auto& e : r.errors) {
    w.begin_object();
    w.key("stage").value(stage_name(e.stage));
    w.key("severity").value(severity_name(e.severity));
    w.key("code").value(code_name(e.code));
    w.key("track").value(e.track);
    w.key("byte_offset").value(e.byte_offset);
    w.key("byte_offset_end").value(e.byte_offset_end);
    w.key("offset_known").value(e.offset_known);
    w.key("detail").value(e.detail);
    w.key("action").value(e.action);
    w.end_object();
  }
  w.end_array();
  w.key("unindexable").begin_array();
  for (const auto& u : r.unindexable) {
    w.begin_object();
    w.key("stream_index").value(u.stream_index);
    w.key("start_ticks").value(u.start_ticks);
    w.key("end_ticks").value(u.end_ticks);
    w.key("reason").value(u.reason);
    w.end_object();
  }
  w.end_array();
  w.end_object();
  return w.str();
}

struct AssetRun {
  FileResult result;
  bool fatal = false;
};

FileResult failed_result(const std::filesystem::path& input, const Error& e) {
  FileResult r;
  r.input_path = input.string();
  r.status = FileStatus::Failed;
  r.errors.push_back(e);
  r.first_error_offset = e.offset_known ? e.byte_offset : -1;
  r.offset_error_bytes = e.offset_uncertainty();
  r.failure_reason = e.detail;
  return r;
}

bool write_sidecar(const std::filesystem::path& output, const FileResult& r,
                   std::string& err) {
  std::filesystem::path side = output;
  side += kSidecarSuffix;
  return write_file_atomic(side, render_sidecar(r), err);
}

}  // namespace

const char* file_status_name(FileStatus s) {
  switch (s) {
    case FileStatus::Ok: return "ok";
    case FileStatus::Failed: return "failed";
    case FileStatus::Interrupted: return "interrupted";
    case FileStatus::Resumed: return "resumed";
  }
  return "unknown";
}

std::vector<std::filesystem::path> discover_inputs(
    const std::filesystem::path& input_dir, std::vector<Error>& rejected) {
  std::vector<std::filesystem::path> out;
  std::error_code ec;
  if (!std::filesystem::is_directory(input_dir, ec)) return out;
  std::vector<std::filesystem::path> all;
  for (const auto& entry : std::filesystem::directory_iterator(input_dir, ec)) {
    if (!entry.is_regular_file()) continue;
    if (!is_media_extension(entry.path())) continue;
    all.push_back(entry.path());
  }
  std::sort(all.begin(), all.end());
  for (const auto& p : all) {
    if (!path_is_within(p, input_dir)) {
      Error e;
      e.file = p.string();
      e.stage = Stage::Open;
      e.severity = Severity::Fatal;
      e.code = Code::PathEscape;
      e.offset_known = false;
      e.detail = "input resolves outside the batch input directory";
      e.action = "skip this file";
      rejected.push_back(e);
      continue;
    }
    out.push_back(p);
  }
  return out;
}

// ---- single asset -------------------------------------------------------

namespace {

AssetRun process_asset(const BatchOptions& options, const std::filesystem::path& input,
                       const std::filesystem::path& output,
                       const std::filesystem::path& checkpoint_dir) {
  AssetRun run;
  FileResult& res = run.result;
  res.input_path = input.string();
  res.output_path = output.string();
  const auto asset_start = std::chrono::steady_clock::now();
  struct Elapsed {
    const std::chrono::steady_clock::time_point& start;
    FileResult& res;
    ~Elapsed() {
      // A resumed asset still costs a scan, a hash and a checkpoint read;
      // report at least one second so the figure is never meaningless.
      const auto secs = std::chrono::duration_cast<std::chrono::seconds>(
                            std::chrono::steady_clock::now() - start)
                            .count();
      if (res.resumed) res.resume_seconds = std::max<int64_t>(1, static_cast<int64_t>(secs));
    }
  } elapsed_guard{asset_start, res};

  Scanner scanner;
  ScanOutcome scan;
  if (!scanner.open(input.string(), options.config.limits, scan)) {
    res.status = FileStatus::Failed;
    res.input_sha256 = scan.input_sha256;
    res.input_bytes = scan.file_size;
    res.errors.push_back(scan.fatal_error);
    res.first_error_offset = scan.fatal_error.offset_known ? scan.fatal_error.byte_offset : -1;
    res.offset_error_bytes = scan.fatal_error.offset_uncertainty();
    res.failure_reason = scan.fatal_error.detail;
    run.fatal = true;
    return run;
  }
  res.input_sha256 = scan.input_sha256;
  res.input_bytes = scan.file_size;
  for (const auto& e : scan.errors) res.errors.push_back(e);
  if (scan.first_damaged_offset >= 0) {
    res.first_error_offset = scan.first_damaged_offset;
    res.offset_error_bytes = scan.offset_uncertainty;
  }

  // ---- resume ------------------------------------------------------------
  const std::string cfg_digest = config_digest(options.config.summary());
  ResumeStore store(checkpoint_dir);
  bool have_checkpoint = false;
  Checkpoint checkpoint;
  if (options.resume) {
    ResumeDecision decision;
    Error rerr;
    store.load_for(input.string(), scan.input_sha256, cfg_digest, output.string(),
                   decision, rerr);
    if (decision.resume) {
      have_checkpoint = true;
      checkpoint = decision.checkpoint;
      res.resumed = true;
      res.resume_seconds = decision.seconds_saved;
      if (checkpoint.stage == JobStage::Verified && !checkpoint.output_sha256.empty()) {
        const auto resume_start = std::chrono::steady_clock::now();
        std::string hex;
        int64_t bytes = 0;
        if (std::filesystem::exists(output) && sha256_file(output, hex, bytes) &&
            hex == checkpoint.output_sha256) {
          // Already finished and unchanged: reuse without touching the file.
          res.status = FileStatus::Resumed;
          res.output_sha256 = hex;
          res.output_bytes = bytes;
          res.packets = checkpoint.packets_written;
          res.repairs = checkpoint.repairs;
          res.decode_errors = checkpoint.errors;
          res.dropped_packets = checkpoint.dropped;
          // Report whole seconds actually spent resuming; a warm cache can
          // complete in well under a second, so the floor is one second only
          // when some measurable work was done.
          const auto elapsed = std::chrono::steady_clock::now() - resume_start;
          res.resume_seconds = std::max<int64_t>(
              1, std::chrono::duration_cast<std::chrono::seconds>(elapsed).count());
          // Evidence files from the first run are still valid; rewrite the
          // sidecar so a deleted one is restored without redoing the work.
          std::string serr;
          res.journal_path = checkpoint.journal_path;
          write_sidecar(output, res, serr);
          run.fatal = false;
          return run;
        }
        // Output changed: the checkpoint no longer describes it.
        res.checkpoint_refused = true;
        res.refusal_reason = "verified output no longer matches its checkpoint";
        have_checkpoint = false;
        res.resumed = false;
        res.resume_seconds = 0;
        Error e;
        e.file = input.string();
        e.stage = Stage::Write;
        e.severity = Severity::Recovered;
        e.code = Code::CheckpointMismatch;
        e.offset_known = false;
        e.detail = "checkpoint says the output was verified, but its hash changed";
        e.action = "rebuild the output from the beginning";
        res.errors.push_back(e);
      }
    } else {
      res.refusal_reason = decision.reason;
      // Only a checkpoint that existed and then failed validation counts as a
      // refusal; "no checkpoint" is the normal first run.
      if (rerr.code != Code::None) {
        res.checkpoint_refused = true;
        res.errors.push_back(rerr);
        store.remove(input.string());
      }
    }
  }
  if (!have_checkpoint) {
    checkpoint = Checkpoint{};
    checkpoint.input_path = input.string();
    checkpoint.input_sha256 = scan.input_sha256;
    checkpoint.input_size = scan.file_size;
    checkpoint.config_digest = cfg_digest;
    checkpoint.output_path = output.string();
    checkpoint.stage = JobStage::Scanned;
  }

  const std::string stem = input.stem().string();
  const auto work = checkpoint_dir / ("parts_" + stem);
  IndexWriter writer;
  std::string err;
  const int64_t expect_payload = have_checkpoint && checkpoint.stage != JobStage::Scanned
                                     ? checkpoint.bytes_written
                                     : 0;
  const int64_t expect_journal = have_checkpoint && checkpoint.stage != JobStage::Scanned
                                     ? checkpoint.packets_written * kJournalRecordBytes
                                     : 0;
  const bool resuming_stream = have_checkpoint && checkpoint.stage == JobStage::Streaming;
  if (!writer.open(work, stem, expect_payload, expect_journal, !resuming_stream, err)) {
    res.status = FileStatus::Failed;
    res.failure_reason = err;
    Error e;
    e.file = input.string();
    e.stage = Stage::Write;
    e.severity = Severity::Fatal;
    e.code = Code::OutputWriteFailed;
    e.offset_known = false;
    e.detail = err;
    e.action = "discard the partial work directory and rerun";
    res.errors.push_back(e);
    run.fatal = true;
    return run;
  }

  // Restore per-stream timeline state so repaired timestamps of the resumed
  // tail continue exactly where the checkpoint left off.
  std::vector<TimelineResult> timelines(scan.tracks.size());
  for (size_t i = 0; i < scan.tracks.size(); ++i) {
    timelines[i].time_base = scan.tracks[i].time_base;
  }
  if (have_checkpoint) {
    for (size_t i = 0; i < checkpoint.stream_indices.size(); ++i) {
      for (size_t t = 0; t < scan.tracks.size(); ++t) {
        if (scan.tracks[t].stream_index == checkpoint.stream_indices[i]) {
          timelines[t].state = checkpoint.per_stream[i];
        }
      }
    }
  }
  std::map<int, size_t> track_of_stream;
  for (size_t i = 0; i < scan.tracks.size(); ++i) {
    track_of_stream[scan.tracks[i].stream_index] = i;
  }
  // On resume the demuxer is replayed from the beginning (seeking into the
  // middle of an MP4 would make packet boundaries ambiguous), but the packets
  // already committed to the journal are skipped by a single global count.
  // A global count keeps the journal byte-identical to an uninterrupted run:
  // per-stream skipping would let the two streams interleave differently and
  // change the output hash.
  int64_t skip_remaining = 0;
  if (have_checkpoint && checkpoint.stage == JobStage::Streaming) {
    skip_remaining = checkpoint.packets_written;
  }
  int64_t packets_skipped = 0;

  int64_t appended = 0;
  int64_t last_consumed_offset = 0;
  std::string stream_error;
  bool aborted = false;
  const int64_t stop_after = options.stop_after_packets > 0
                                 ? options.stop_after_packets
                                 : 0;
  std::vector<int64_t> per_stream_packets(scan.tracks.size(), 0);
  bool ok = scanner.stream(
      [&](int si, const AVPacket& pkt, std::string& why) {
        auto it = track_of_stream.find(si);
        if (it == track_of_stream.end()) return true;
        if (skip_remaining > 0) {
          --skip_remaining;
          ++packets_skipped;
          return true;
        }
        const size_t ti = it->second;
        TimelineOptions topt;
        topt.time_base = scan.tracks[ti].time_base;
        topt.max_repairs = options.config.limits.max_timeline_errors;
        const bool pushed = timeline_push(topt, timelines[ti], pkt.dts, pkt.pts,
                                          pkt.duration, pkt.pos, pkt.size, pkt.flags,
                           [&](const TimelineSample& s) {
                             JournalEntry je;
                             je.stream_index = si;
                             je.flags = pkt.flags;
                             je.size = static_cast<uint32_t>(pkt.size);
                             je.dts = s.dts;
                             je.pts = s.pts;
                             je.duration = s.duration;
                             je.src_offset = s.byte_offset;
                             if (!writer.append(je, pkt.data, why)) {
                               stream_error = why;
                               aborted = true;
                             }
                             ++appended;
                           });
        if (!pushed) {
          why = timelines[ti].failure;
          stream_error = why;
          timeline_set_failure_range(timelines[ti], pkt.pos, pkt.size);
          aborted = true;
          return false;
        }
        ++per_stream_packets[ti];
        if (pkt.pos >= 0) last_consumed_offset = pkt.pos + pkt.size;
        // The cut is global so that a resumed run replays the exact same
        // journal order an uninterrupted run would have produced.
        if (stop_after > 0 && appended >= stop_after) {
          why = "stop-after-packet limit reached (test hook)";
          stream_error = why;
          aborted = true;
          return false;
        }
        return true;
      },
      scan);
  (void)ok;

  for (auto& t : timelines) {
    if (!t.ok) {
      Error e;
      e.file = input.string();
      e.stage = Stage::Timeline;
      e.severity = Severity::Fatal;
      e.code = Code::TimestampAmbiguous;
      e.offset_known = t.failure_offset >= 0 || t.stats.first_input_offset >= 0;
      if (t.failure_offset >= 0) {
        e.set_range(t.failure_offset, t.failure_offset_end);
        e.last_confirmed = t.failure_offset;
      } else {
        e.set_range(t.stats.first_input_offset, 0);
      }
      e.track = -1;
      e.detail = t.failure;
      e.action = "mark asset failed; timestamps cannot be repaired unambiguously";
      res.errors.push_back(e);
      res.failure_reason = t.failure;
      run.fatal = true;
    }
    res.repairs += static_cast<int64_t>(t.repairs.size());
    if (t.failure_offset >= 0 &&
        (res.first_error_offset < 0 || t.failure_offset < res.first_error_offset)) {
      res.first_error_offset = t.failure_offset;
      res.offset_error_bytes = t.failure_offset_end - t.failure_offset;
    }
  }

  // A stop requested by the packet callback (the test hook) is a resumable
  // interruption; a timeline failure or a truncated tail is not.
  const bool deliberately_stopped =
      aborted && stop_after > 0 && !run.fatal && scan.stopped_by_sink &&
      stream_error.find("stop-after") != std::string::npos;
  if (deliberately_stopped) {
    // Deliberate interruption: persist a checkpoint with the committed counts
    // so the next run resumes from here.
    writer.flush(err);
    Checkpoint cp = checkpoint;
    cp.stage = JobStage::Streaming;
    cp.input_offset = last_consumed_offset;
    cp.packets_written = writer.packets();
    cp.bytes_written = writer.payload_bytes();
    cp.output_path = output.string();
    cp.repairs = res.repairs;
    cp.errors = static_cast<int64_t>(res.errors.size());
    cp.dropped = scan.dropped_packets;
    cp.written_at = iso8601_utc(now_seconds());
    cp.stream_indices.clear();
    cp.per_stream.clear();
    for (size_t i = 0; i < timelines.size(); ++i) {
      cp.stream_indices.push_back(scan.tracks[i].stream_index);
      cp.per_stream.push_back(timelines[i].state);
    }
    std::string serr;
    if (store.save(cp, serr)) {
      res.status = FileStatus::Interrupted;
      res.failure_reason = stream_error;
      return run;
    }
    res.failure_reason = "cannot save checkpoint: " + serr;
    res.status = FileStatus::Failed;
    run.fatal = true;
    return run;
  }

  if (run.fatal) {
    res.status = FileStatus::Failed;
    // Record the failure so a later --resume explains why it will not reuse
    // this asset, instead of repeating the whole scan silently.
    Checkpoint failed = checkpoint;
    failed.stage = JobStage::Failed;
    failed.repairs = res.repairs;
    failed.errors = static_cast<int64_t>(res.errors.size());
    failed.written_at = iso8601_utc(now_seconds());
    std::string ignored;
    store.save(failed, ignored);
    return run;
  }
  if (aborted) {
    res.status = FileStatus::Failed;
    res.failure_reason = stream_error.empty() ? "streaming aborted" : stream_error;
    return run;
  }
  if (writer.packets() == 0) {
    res.status = FileStatus::Failed;
    res.failure_reason = "no packets survived scanning";
    Error e;
    e.file = input.string();
    e.stage = Stage::Scan;
    e.severity = Severity::Fatal;
    e.code = Code::EmptyOutput;
    e.offset_known = false;
    e.detail = res.failure_reason;
    e.action = "mark asset failed";
    res.errors.push_back(e);
    run.fatal = true;
    return run;
  }

  // ---- mux into a partial file ------------------------------------------
  const std::filesystem::path partial = output;
  std::filesystem::path partial_path = partial;
  partial_path += ".partial";
  MuxOptions mux;
  mux.config = options.config;
  MuxReport mux_report;
  Error mux_err;
  if (!mux_output(input, writer.payload_path(), writer.journal_path(), partial_path, mux,
                  mux_report, mux_err)) {
    res.status = FileStatus::Failed;
    res.failure_reason = mux_err.detail;
    res.errors.push_back(mux_err);
    run.fatal = true;
    std::filesystem::remove(partial_path);
    return run;
  }

  // ---- independent verification -----------------------------------------
  VerificationReport vrep;
  Error verr;
  const bool verified = verify_output(partial_path, writer.journal_path(), input,
                                      options.config, vrep, verr);
  for (const auto& e : vrep.errors) res.errors.push_back(e);
  res.decode_errors = vrep.decode_errors;
  res.index_entries = vrep.index_entries;
  res.keyframes = vrep.keyframes;
  res.unindexable = vrep.unindexable;
  res.packets = writer.packets();
  res.dropped_packets = scan.dropped_packets;
  res.output_sha256 = vrep.output_sha256;
  res.output_bytes = vrep.output_bytes;
  if (vrep.first_error_offset >= 0 &&
      (res.first_error_offset < 0 || vrep.first_error_offset < res.first_error_offset)) {
    res.first_error_offset = vrep.first_error_offset;
    res.offset_error_bytes = vrep.offset_error_bytes;
  }
  if (!verified || !vrep.ok) {
    res.status = FileStatus::Failed;
    res.failure_reason = verr.detail.empty() ? "output verification failed" : verr.detail;
    if (!verr.detail.empty()) res.errors.push_back(verr);
    run.fatal = true;
    std::filesystem::remove(partial_path);
    return run;
  }

  // ---- atomic install ----------------------------------------------------
  std::error_code ec;
  std::filesystem::create_directories(output.parent_path(), ec);
  std::filesystem::rename(partial_path, output, ec);
  if (ec) {
    res.status = FileStatus::Failed;
    res.failure_reason = "atomic rename failed: " + ec.message();
    Error e;
    e.file = output.string();
    e.stage = Stage::Write;
    e.severity = Severity::Fatal;
    e.code = Code::OutputWriteFailed;
    e.offset_known = false;
    e.detail = res.failure_reason;
    e.action = "the previous output is untouched";
    res.errors.push_back(e);
    run.fatal = true;
    return run;
  }
  res.status = FileStatus::Ok;
  res.resumed = have_checkpoint;
  res.journal_path = writer.journal_path().string();

  // The sidecar carries the evidence verify needs after the fact.
  std::string serr;
  if (!write_sidecar(output, res, serr)) {
    // Non-fatal: the output itself is verified; only the evidence file is
    // missing, and that is reported in the next verify run.
    if (options.verbose) std::fprintf(stderr, "mediaqc: sidecar write failed: %s\n", serr.c_str());
  }

  // The payload is no longer needed once the output is installed; the journal
  // is small and stays as the byte-offset map used by the verify subcommand.
  std::filesystem::remove(writer.payload_path(), ec);
  writer.close_files();

  // Mark the checkpoint verified so a repeated run is a no-op.
  Checkpoint cp = checkpoint;
  cp.stage = JobStage::Verified;
  cp.output_sha256 = res.output_sha256;
  cp.journal_path = writer.journal_path().string();
  cp.packets_written = writer.packets();
  cp.bytes_written = writer.payload_bytes();
  cp.repairs = res.repairs;
  cp.errors = static_cast<int64_t>(res.errors.size());
  cp.dropped = res.dropped_packets;
  cp.written_at = iso8601_utc(now_seconds());
  cp.stream_indices.clear();
  cp.per_stream.clear();
  for (size_t i = 0; i < timelines.size(); ++i) {
    cp.stream_indices.push_back(scan.tracks[i].stream_index);
    cp.per_stream.push_back(timelines[i].state);
  }
  store.save(cp, serr);
  return run;
}

void aggregate(BatchResult& batch) {
  BatchTotals& t = batch.totals;
  t = BatchTotals{};
  std::string sha_input;
  for (const auto& f : batch.files) {
    ++t.files_total;
    switch (f.status) {
      case FileStatus::Ok: ++t.files_ok; break;
      case FileStatus::Resumed: ++t.files_ok; ++t.files_resumed; break;
      case FileStatus::Failed: ++t.files_failed; break;
      case FileStatus::Interrupted: ++t.files_interrupted; break;
    }
    if (f.checkpoint_refused) ++t.checkpoint_refusals;
    // Report the first damaged byte of the first asset (in discovery order)
    // that has a localised error.  Offsets from different files are not
    // comparable, so taking a global minimum would be meaningless.
    if (t.first_error_offset < 0 && f.first_error_offset >= 0) {
      t.first_error_offset = f.first_error_offset;
      t.offset_error_bytes = f.offset_error_bytes;
    }
    t.resume_seconds += f.resume_seconds;
    t.index_entries += f.index_entries;
    t.decode_errors += f.decode_errors;
    t.repairs += f.repairs;
    t.packets += f.packets;
    sha_input += f.output_sha256.empty() ? "-" : f.output_sha256;
    sha_input += '\n';
  }
  Sha256 h;
  h.update("mediaqc-outputs-v1\n");
  h.update(sha_input);
  t.output_sha256 = h.hex_digest();
  if (t.files_failed > 0) batch.exit_code = 1;
  else if (t.files_interrupted > 0) batch.exit_code = 0;
  else batch.exit_code = 0;
}

}  // namespace

BatchResult run_batch(const BatchOptions& options) {
  BatchResult batch;
  const auto start = std::chrono::steady_clock::now();
  std::error_code ec;
  if (!std::filesystem::is_directory(options.input_dir, ec)) {
    batch.exit_code = 2;
    batch.error = "input directory does not exist: " + options.input_dir.string();
    return batch;
  }
  std::filesystem::create_directories(options.output_dir, ec);
  if (ec) {
    batch.exit_code = 2;
    batch.error = "cannot create output directory: " + ec.message();
    return batch;
  }
  const auto checkpoint_dir = options.checkpoint_dir.empty()
                                  ? options.output_dir / ".mediaqc"
                                  : options.checkpoint_dir;
  std::filesystem::create_directories(checkpoint_dir, ec);
  if (ec) {
    batch.exit_code = 2;
    batch.error = "cannot create checkpoint directory: " + ec.message();
    return batch;
  }

  std::vector<Error> rejected;
  const auto inputs = discover_inputs(options.input_dir, rejected);
  for (const auto& e : rejected) {
    FileResult r = failed_result(e.file, e);
    batch.files.push_back(r);
  }
  for (const auto& input : inputs) {
    const auto output = options.output_dir / output_name_for(input);
    AssetRun run;
    try {
      run = process_asset(options, input, output, checkpoint_dir);
    } catch (const std::exception& ex) {
      Error e;
      e.file = input.string();
      e.stage = Stage::Scan;
      e.severity = Severity::Fatal;
      e.code = Code::HeaderUnparsable;
      e.offset_known = false;
      e.detail = std::string("unhandled error: ") + ex.what();
      e.action = "mark asset failed and continue the batch";
      run.result = failed_result(input, e);
      run.fatal = true;
    }
    batch.files.push_back(run.result);
  }
  aggregate(batch);
  batch.elapsed_seconds =
      std::chrono::duration_cast<std::chrono::seconds>(std::chrono::steady_clock::now() -
                                                       start)
          .count();
  return batch;
}

// ---- verify -------------------------------------------------------------

namespace {

bool load_sidecar(const std::filesystem::path& output, FileResult& r) {
  std::filesystem::path side = output;
  side += kSidecarSuffix;
  std::string text;
  if (!read_file(side, text, 1 << 20)) return false;
  // Minimal extraction: the sidecar is written by us and is stable, so the
  // fields verify needs are pulled by key rather than a full JSON parser.
  auto value_of = [&](const std::string& key, size_t from) -> std::string {
    const std::string pat = "\"" + key + "\":";
    size_t at = text.find(pat, from);
    if (at == std::string::npos) return {};
    at += pat.size();
    if (at < text.size() && text[at] == '"') {
      size_t end = at + 1;
      while (end < text.size() && text[end] != '"') {
        if (text[end] == '\\') ++end;
        ++end;
      }
      return text.substr(at + 1, end - at - 1);
    }
    size_t end = at;
    while (end < text.size() && text[end] != ',' && text[end] != '}') ++end;
    return text.substr(at, end - at);
  };
  auto num = [&](const std::string& key) -> int64_t {
    const std::string v = value_of(key, 0);
    if (v.empty()) return -1;
    try {
      return std::stoll(v);
    } catch (...) {
      return -1;
    }
  };
  r.input_path = value_of("input", 0);
  r.input_sha256 = value_of("input_sha256", 0);
  r.input_bytes = num("input_bytes");
  r.output_sha256 = value_of("output_sha256", 0);
  r.first_error_offset = num("first_error_offset_bytes");
  r.offset_error_bytes = num("offset_error_bytes");
  r.decode_errors = num("decode_errors");
  r.index_entries = num("index_entries");
  return true;
}

}  // namespace

BatchResult run_verify(const VerifyOptions& options) {
  BatchResult batch;
  std::error_code ec;
  if (!std::filesystem::is_directory(options.input_dir, ec)) {
    batch.exit_code = 2;
    batch.error = "input directory does not exist: " + options.input_dir.string();
    return batch;
  }
  std::vector<std::filesystem::path> outputs;
  for (const auto& entry : std::filesystem::directory_iterator(options.input_dir, ec)) {
    if (!entry.is_regular_file()) continue;
    if (!is_media_extension(entry.path())) continue;
    outputs.push_back(entry.path());
  }
  std::sort(outputs.begin(), outputs.end());
  bool any_verify_failure = false;
  for (const auto& output : outputs) {
    FileResult r;
    r.output_path = output.string();
    r.input_path = output.string();
    bool have_sidecar = load_sidecar(output, r);
    if (!have_sidecar) {
      // Without a sidecar the verifier still checks the output on its own;
      // only the input-offset evidence is unavailable.
      r.first_error_offset = -1;
    } else {
      // Keep input damage evidence separate: verification is about the output.
      r.input_evidence_offset = r.first_error_offset;
      r.input_evidence_uncertainty = r.offset_error_bytes;
      r.first_error_offset = -1;
      r.offset_error_bytes = 0;
    }
    std::filesystem::path journal;
    std::filesystem::path input;
    std::filesystem::path side = output;
    side += kSidecarSuffix;
    std::string text;
    if (read_file(side, text, 1 << 20)) {
      const std::string pat = "\"input\":\"";
      size_t at = text.find(pat);
      if (at != std::string::npos) {
        at += pat.size();
        size_t end = text.find('"', at);
        if (end != std::string::npos) input = text.substr(at, end - at);
      }
    }
    if (!input.empty()) {
      std::filesystem::path work = output.parent_path() / ".mediaqc" /
                                   ("parts_" + std::filesystem::path(r.input_path).stem().string());
      journal = work / (std::filesystem::path(r.input_path).stem().string() + ".journal");
      if (!std::filesystem::exists(journal)) journal.clear();
    } else {
      journal.clear();
    }
    VerificationReport vrep;
    Error fatal;
    const bool ok = verify_output(output, journal, input, options.config, vrep, fatal);
    r.output_sha256 = vrep.output_sha256;
    r.output_bytes = vrep.output_bytes;
    r.packets = vrep.packets;
    r.index_entries = vrep.index_entries;
    r.decode_errors = vrep.decode_errors;
    r.keyframes = vrep.keyframes;
    for (const auto& e : vrep.errors) r.errors.push_back(e);
    if (vrep.first_error_offset >= 0) {
      r.first_error_offset = vrep.first_error_offset;
      r.offset_error_bytes = vrep.offset_error_bytes;
    }
    if (!ok) {
      r.status = FileStatus::Failed;
      r.failure_reason = fatal.detail.empty() ? "verification failed" : fatal.detail;
      any_verify_failure = true;
    } else {
      r.status = FileStatus::Ok;
    }
    batch.files.push_back(r);
  }
  aggregate(batch);
  batch.totals.files_failed = 0;
  for (const auto& f : batch.files) {
    if (f.status == FileStatus::Failed) ++batch.totals.files_failed;
  }
  if (any_verify_failure) batch.exit_code = 3;
  else batch.exit_code = 0;
  return batch;
}

// ---- selftest -----------------------------------------------------------

BatchResult run_selftest(const SelftestOptions& options) {
  BatchResult batch;
  FixtureSet set = generate_fixtures(options.work_dir, options.ffmpeg_binary);
  if (!set.ok) {
    batch.exit_code = 2;
    batch.error = set.error;
    return batch;
  }
  BatchOptions bo;
  bo.config = options.config;
  bo.input_dir = set.input_dir;
  bo.output_dir = set.output_dir;
  bo.json = options.json;
  bo.resume = true;
  // The preseeded stale checkpoint lives next to the fixtures.
  bo.checkpoint_dir = set.checkpoint_dir;
  batch = run_batch(bo);
  // Compare against the fixture expectations: an asset designed to succeed
  // must be ok, and one designed to fail must fail with a localised error.
  std::map<std::string, bool> expect_ok;
  for (const auto& f : set.files) expect_ok[f.name] = f.expect_ok;
  int64_t deviations = 0;
  for (const auto& f : batch.files) {
    const std::string name = std::filesystem::path(f.input_path).filename().string();
    const auto it = expect_ok.find(name);
    if (it == expect_ok.end()) continue;
    const bool ok = f.status == FileStatus::Ok || f.status == FileStatus::Resumed;
    if (ok != it->second) {
      ++deviations;
      batch.error += (batch.error.empty() ? "" : "; ") + name + " behaved unexpectedly";
    }
  }
  batch.totals.files_ok = 0;
  batch.totals.files_failed = 0;
  batch.totals.files_resumed = 0;
  for (const auto& f : batch.files) {
    if (f.status == FileStatus::Ok) ++batch.totals.files_ok;
    else if (f.status == FileStatus::Resumed) { ++batch.totals.files_ok; ++batch.totals.files_resumed; }
    else ++batch.totals.files_failed;
  }
  // selftest passes when every fixture matches its expectation, even though
  // some of them are supposed to fail (their failures are the test).
  batch.exit_code = deviations > 0 ? 1 : 0;
  return batch;
}

// ---- JSON rendering -----------------------------------------------------

std::string render_json(const BatchResult& result, bool verify_mode) {
  JsonWriter w;
  w.begin_object();
  w.key("mode").value(verify_mode ? "verify" : "batch");
  w.key("files_total").value(result.totals.files_total);
  w.key("files_ok").value(result.totals.files_ok);
  w.key("files_failed").value(result.totals.files_failed);
  w.key("files_resumed").value(result.totals.files_resumed);
  w.key("files_interrupted").value(result.totals.files_interrupted);
  w.key("checkpoint_refusals").value(result.totals.checkpoint_refusals);
  w.key("first_error_offset_bytes").value(result.totals.first_error_offset);
  w.key("offset_error_bytes").value(result.totals.offset_error_bytes);
  w.key("resume_seconds").value(result.totals.resume_seconds);
  w.key("output_sha256").value(result.totals.output_sha256);
  w.key("index_entries").value(result.totals.index_entries);
  w.key("decode_errors").value(result.totals.decode_errors);
  w.key("repairs").value(result.totals.repairs);
  w.key("packets").value(result.totals.packets);
  w.key("elapsed_seconds").value(result.elapsed_seconds);
  w.key("exit_code").value(result.exit_code);
  if (!result.error.empty()) w.key("error").value(result.error);
  w.key("files").begin_array();
  for (const auto& f : result.files) {
    w.begin_object();
    w.key("input").value(f.input_path);
    w.key("output").value(f.output_path);
    w.key("status").value(file_status_name(f.status));
    w.key("input_sha256").value(f.input_sha256);
    w.key("output_sha256").value(f.output_sha256);
    w.key("packets").value(f.packets);
    w.key("index_entries").value(f.index_entries);
    w.key("keyframes").value(f.keyframes);
    w.key("decode_errors").value(f.decode_errors);
    w.key("repairs").value(f.repairs);
    w.key("first_error_offset_bytes").value(f.first_error_offset);
    w.key("offset_error_bytes").value(f.offset_error_bytes);
    w.key("resume_seconds").value(f.resume_seconds);
    w.key("input_evidence_offset_bytes").value(f.input_evidence_offset);
    w.key("resumed").value(f.resumed);
    w.key("checkpoint_refused").value(f.checkpoint_refused);
    if (!f.refusal_reason.empty()) w.key("refusal_reason").value(f.refusal_reason);
    if (!f.failure_reason.empty()) w.key("failure_reason").value(f.failure_reason);
    w.key("errors").begin_array();
    for (const auto& e : f.errors) {
      w.begin_object();
      w.key("stage").value(stage_name(e.stage));
      w.key("severity").value(severity_name(e.severity));
      w.key("code").value(code_name(e.code));
      w.key("track").value(e.track);
      w.key("byte_offset").value(e.byte_offset);
      w.key("byte_offset_end").value(e.byte_offset_end);
      w.key("offset_known").value(e.offset_known);
      w.key("detail").value(e.detail);
      w.key("action").value(e.action);
      w.end_object();
    }
    w.end_array();
    w.end_object();
  }
  w.end_array();
  w.end_object();
  return w.str();
}

}  // namespace mediaqc
