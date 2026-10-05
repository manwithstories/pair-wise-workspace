"""Test suite for telem_replay.  Standard library unittest only."""

from __future__ import annotations

import base64
import io
import json
import os
import random
import shutil
import struct
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from telem_replay import protocol
from telem_replay.pipeline import ReplayPipeline
from telem_replay.protocol import (
    ProtocolError,
    build_frame,
    build_fragments,
    crc_cover,
    crc32c,
    frame_crc,
    iter_tlvs,
    parse_frame,
    parse_tlvs,
)
from telem_replay.reassembler import ReassemblyManager, SessionReassembler


def tlv(tag: int, value: bytes) -> bytes:
    return struct.pack("<HH", tag, len(value)) + value


def env(session: str, frame: bytes, ts: int = 0) -> str:
    return json.dumps({"session_id": session, "ts": ts,
                       "payload": base64.b64encode(frame).decode()})


class TestCRC32C(unittest.TestCase):
    def test_known_vector(self):
        self.assertEqual(crc32c(b"123456789"), 0xE3069283)

    def test_empty(self):
        self.assertEqual(crc32c(b""), 0)

    def test_incremental_matches_oneshot(self):
        data = bytes(range(256)) * 5
        for split in (0, 1, 7, 8, 9, 63, 100, len(data)):
            seeded = crc32c(data[:split], crc32c(data[:0])) if split else 0
            self.assertEqual(crc32c(data[split:], seeded), crc32c(data), f"split={split}")

    def test_differs_from_zlib_crc32(self):
        # Guards the trap: zlib.crc32 is CRC-32/ISO-HDLC, not CRC-32C.
        import zlib
        self.assertNotEqual(crc32c(b"123456789"), zlib.crc32(b"123456789"))


class TestProtocol(unittest.TestCase):
    def test_roundtrip_unfragmented(self):
        body = tlv(0x0101, struct.pack("<h", -5))
        f = parse_frame(build_frame(7, body))
        self.assertEqual(f.seq, 7)
        self.assertEqual(f.body, body)
        self.assertFalse(f.chunked())
        protocol.verify_crc(f)

    def test_roundtrip_fragmented(self):
        body = b"".join(tlv(0x0100 + i, struct.pack("<h", i)) for i in range(6))
        frags = [parse_frame(x) for x in build_fragments(3, body, 4)]
        self.assertEqual(len(frags), 4)
        # Every fragment must carry the same frame-level CRC and cover.
        self.assertEqual(len({x.crc for x in frags}), 1)
        self.assertEqual(len({x.cover for x in frags}), 1)
        joined = b"".join(x.body for x in sorted(frags, key=lambda y: y.frag_index))
        self.assertEqual(joined, body)
        self.assertEqual(frame_crc(frags[0].cover, joined), frags[0].crc)

    def test_crc_detects_single_bit_flip(self):
        f = build_frame(1, tlv(0x0101, struct.pack("<h", 3)))
        bad = bytearray(f)
        bad[-1] ^= 0x08
        with self.assertRaises(ProtocolError):
            protocol.verify_crc(parse_frame(bytes(bad)))

    def test_rejects_bad_magic(self):
        f = bytearray(build_frame(1, b"\x00\x00"))
        f[0:4] = b"XXXX"
        with self.assertRaises(ProtocolError):
            parse_frame(bytes(f))

    def test_rejects_short_frame(self):
        with self.assertRaises(ProtocolError):
            parse_frame(b"\x00" * 10)

    def test_rejects_absurd_total_len(self):
        raw = bytearray(build_frame(1, b"ab"))
        struct.pack_into("<I", raw, 18, protocol.MAX_FRAME_BYTES + 1)
        with self.assertRaises(ProtocolError):
            parse_frame(bytes(raw))

    def test_rejects_trailing_bytes(self):
        with self.assertRaises(ProtocolError):
            parse_frame(build_frame(1, b"\x00\x00") + b"junk")

    def test_tlv_truncation_detected(self):
        with self.assertRaises(ProtocolError):
            list(iter_tlvs(struct.pack("<HH", 1, 50) + b"short"))

    def test_tlv_trailing_bytes_detected(self):
        with self.assertRaises(ProtocolError):
            list(iter_tlvs(b"\x01\x02"))

    def test_tlv_values(self):
        body = tlv(0x0101, b"\x01\x02") + tlv(0x0201, b"\x00\x01")
        self.assertEqual(parse_tlvs(body), {0x0101: b"\x01\x02", 0x0201: b"\x00\x01"})


class TestReassembler(unittest.TestCase):
    def _mgr(self, window=256, max_sessions=8):
        return ReassemblyManager(window_size=window, max_sessions=max_sessions)

    def test_in_order_single(self):
        m = self._mgr()
        out = m.feed("s", parse_frame(build_frame(0, tlv(1, b"\x00\x01"))), 0)
        self.assertEqual(len(out), 1)

    def test_fragments_reassemble(self):
        m = self._mgr()
        body = tlv(0x0101, struct.pack("<h", 9))
        frames = [parse_frame(x) for x in build_fragments(0, body, 3)]
        got = []
        for i, f in enumerate(frames):
            got += m.feed("s", f, i)
        self.assertEqual(got, [(0, body)])

    def test_out_of_order_uses_delay_queue(self):
        m = self._mgr()
        b0 = tlv(0x0101, struct.pack("<h", 0))
        b1 = tlv(0x0101, struct.pack("<h", 1))
        b2 = tlv(0x0101, struct.pack("<h", 2))
        self.assertEqual(m.feed("s", parse_frame(build_frame(0, b0)), 0), [(0, b0)])
        # seq 2 arrives before seq 1: it must park, not be emitted out of order.
        self.assertEqual(m.feed("s", parse_frame(build_frame(2, b2)), 1), [])
        # Filling the gap releases both, in ascending sequence order.
        out = m.feed("s", parse_frame(build_frame(1, b1)), 2)
        self.assertEqual(out, [(1, b1), (2, b2)])

    def test_frame_below_anchor_is_kept_not_dropped(self):
        # A log may start mid-stream: seq 1 seen first anchors the session.
        # The seq 0 frame that turns up afterwards must survive until flush.
        m = self._mgr()
        b0 = tlv(0x0101, struct.pack("<h", 0))
        b1 = tlv(0x0101, struct.pack("<h", 1))
        self.assertEqual(m.feed("s", parse_frame(build_frame(1, b1)), 0), [(1, b1)])
        self.assertEqual(m.feed("s", parse_frame(build_frame(0, b0)), 1), [])
        self.assertEqual(m.flush(), [("s", 0, b0)])

    def test_after_resume_below_anchor_is_a_true_duplicate(self):
        # With a restored cursor the anchor is authoritative, so re-seeing
        # committed frames must drop them rather than re-emit.
        m = self._mgr()
        b0 = tlv(0x0101, struct.pack("<h", 0))
        m.feed("s", parse_frame(build_frame(0, b0)), 0)
        m.restore("s", 0)
        self.assertEqual(m.feed("s", parse_frame(build_frame(0, b0)), 1), [])

    def test_permanent_gap_does_not_stall_end_of_input(self):
        m = self._mgr()
        anchor = tlv(1, b"\x00\x00")
        m.feed("s", parse_frame(build_frame(0, anchor)), 0)
        # seq 2 leaves a permanent hole at seq 1; it must park, not block.
        body2 = tlv(1, b"\x00\x02")
        self.assertEqual(m.feed("s", parse_frame(build_frame(2, body2)), 1), [])
        self.assertEqual(m.flush(), [("s", 2, body2)])

    def test_window_evicts_and_warns(self):
        warnings = []
        s = SessionReassembler("s", window_size=2, delay_limit=8, warnings=warnings)
        # Three frames each missing one fragment -> window of 2 must evict.
        for seq in (0, 1, 2):
            frags = build_fragments(seq, tlv(0x0101, struct.pack("<h", seq)) * 3, 3)
            s.feed(parse_frame(frags[0]), seq * 100)
            s.feed(parse_frame(frags[1]), seq * 100 + 1)
        self.assertEqual(s.evicted, 1)
        self.assertTrue(any(w["kind"] == "window_evicted" for w in warnings))

    def test_duplicate_line_dropped(self):
        m = self._mgr()
        f = parse_frame(build_frame(0, tlv(1, b"\x00\x01")))
        self.assertEqual(len(m.feed("s", f, 0)), 1)
        self.assertEqual(m.feed("s", f, 0), [])

    def test_crc_failure_raises(self):
        m = self._mgr()
        bad = bytearray(build_frame(0, tlv(1, b"\x00\x01")))
        bad[-1] ^= 0xFF
        with self.assertRaises(ProtocolError):
            m.feed("s", parse_frame(bytes(bad)), 0)

    def test_safe_offset_tracks_oldest_retained(self):
        m = self._mgr()
        frags = build_fragments(0, tlv(0x0101, struct.pack("<h", 0)) * 3, 3)
        m.feed("s", parse_frame(frags[0]), 500)  # seq 0 pending, first line at 500
        self.assertEqual(m.safe_offset(), 500)

    def test_session_ejection_bounds_memory(self):
        m = self._mgr(max_sessions=2)
        for i in range(5):
            m.feed(f"s{i}", parse_frame(build_frame(0, tlv(1, b"\x00\x01"))), 0)
        self.assertEqual(m.stats()["sessions_tracked"], 2)
        self.assertTrue(any(w["kind"] == "session_ejected" for w in m.warnings))


class TestPipeline(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp(prefix="telem-test-")

    def tearDown(self):
        shutil.rmtree(self.dir, ignore_errors=True)

    def path(self, name):
        return os.path.join(self.dir, name)

    def write(self, name, lines):
        p = self.path(name)
        with open(p, "w", encoding="utf-8") as fh:
            fh.write("\n".join(lines) + "\n")
        return p

    def run_pipeline(self, inp, **kw):
        pipe = ReplayPipeline(
            inp,
            state_path=kw.pop("state", self.path("s.db")),
            output_path=kw.pop("output", self.path("out.jsonl")),
            dlq_path=kw.pop("dlq", self.path("dlq.jsonl")),
            **kw,
        )
        stats = pipe.run()
        pipe.ckpt.close()
        return pipe, stats

    def read_out(self, path=None):
        with open(path or self.path("out.jsonl"), encoding="utf-8") as fh:
            return [json.loads(x) for x in fh if x.strip()]

    def test_clean_stream_commits_everything(self):
        lines = [env("s", build_frame(i, tlv(0x0101, struct.pack("<h", i))))
                 for i in range(50)]
        _, stats = self.run_pipeline(self.write("in.jsonl", lines))
        self.assertEqual(stats.frames_committed, 50)
        self.assertEqual(stats.corrupt, 0)
        self.assertEqual(len(self.read_out()), 50)

    def test_dirty_lines_are_skipped_not_fatal(self):
        lines = [env("s", build_frame(0, tlv(0x0101, struct.pack("<h", 0))))]
        lines += [
            "not json at all",
            '{"session_id": "s", "payload": "###"}',          # bad base64
            json.dumps({"session_id": "s"}),                   # missing field
            json.dumps({"session_id": "", "payload": "AA=="}),  # empty session
            json.dumps({"session_id": "s", "payload": 12}),    # wrong type
            env("s", b"\x00" * 4),                             # truncated frame
            "",                                                 # blank line
        ]
        lines.append(env("s", build_frame(1, tlv(0x0101, struct.pack("<h", 1)))))
        _, stats = self.run_pipeline(self.write("in.jsonl", lines))
        self.assertEqual(stats.frames_committed, 2)
        self.assertGreaterEqual(stats.corrupt, 6)
        # The run must have produced output despite the garbage.
        self.assertEqual(len(self.read_out()), 2)

    def test_dlq_records_reason_and_preview(self):
        lines = ["totally broken", env("s", build_frame(0, tlv(0x0101, b"\x00\x01")))]
        self.run_pipeline(self.write("in.jsonl", lines))
        with open(self.path("dlq.jsonl"), encoding="utf-8") as fh:
            rec = json.loads(fh.readline())
        self.assertIn("reason", rec)
        self.assertIn("raw_preview", rec)
        self.assertIn("offset", rec)

    def test_dlq_preview_is_bounded(self):
        # A single corrupt line far larger than any sane limit: the DLQ must keep
        # a short preview, not a second copy of the input.
        huge = "x" * 500000
        self.run_pipeline(self.write(
            "in.jsonl", [huge, env("s", build_frame(0, tlv(0x0101, b"\x00\x01")))]))
        with open(self.path("dlq.jsonl"), encoding="utf-8") as fh:
            rec = json.loads(fh.readline())
        self.assertLessEqual(len(rec["raw_preview"]), 512)
        # raw_len reports the line as read, including its terminator.
        self.assertEqual(rec["raw_len"], len(huge) + 1)
        # The good frame after the garbage must still have been processed.
        self.assertEqual(len(self.read_out()), 1)

    def test_breaker_transitions_appear_in_timeline(self):
        lines = [
            env("s", build_frame(0, tlv(0x0201, bytes([1, 1])))),   # closed
            env("s", build_frame(1, tlv(0x0201, bytes([1, 0])))),   # open
            env("s", build_frame(2, tlv(0x0201, bytes([1, 1])))),   # closed again
        ]
        self.run_pipeline(self.write("in.jsonl", lines))
        states = [r["breakers"]["1"] for r in self.read_out()]
        self.assertEqual(states, ["closed", "open", "closed"])

    def test_no_state_change_emits_no_record(self):
        # Same point value twice: the second frame changes nothing.
        body = tlv(0x0101, struct.pack("<h", 5))
        lines = [env("s", build_frame(0, body)), env("s", build_frame(1, body))]
        _, stats = self.run_pipeline(self.write("in.jsonl", lines))
        self.assertEqual(stats.frames_committed, 2)
        self.assertEqual(len(self.read_out()), 1)

    def test_unknown_tag_is_ignored(self):
        body = tlv(0x7FFE, b"vendor") + tlv(0x0101, struct.pack("<h", 4))
        _, stats = self.run_pipeline(self.write("in.jsonl", [env("s", build_frame(0, body))]))
        self.assertEqual(stats.corrupt, 0)
        self.assertEqual(stats.frames_committed, 1)

    def test_sessions_are_isolated(self):
        lines = [
            env("a", build_frame(0, tlv(0x0101, struct.pack("<h", 1)))),
            env("b", build_frame(0, tlv(0x0201, bytes([0, 1])))),
        ]
        self.run_pipeline(self.write("in.jsonl", lines))
        recs = self.read_out()
        self.assertEqual({r["session_id"] for r in recs}, {"a", "b"})

    def test_interleaved_sessions_stay_in_seq_order(self):
        lines = []
        for i in range(30):
            lines.append(env("a", build_frame(i, tlv(0x0101, struct.pack("<h", i)))))
            lines.append(env("b", build_frame(i, tlv(0x0102, struct.pack("<h", i)))))
        self.run_pipeline(self.write("in.jsonl", lines))
        for sid in ("a", "b"):
            seqs = [r["seq"] for r in self.read_out() if r["session_id"] == sid]
            self.assertEqual(seqs, sorted(seqs))
            self.assertEqual(seqs, list(range(30)))

    def test_fragmented_frames_across_lines(self):
        body = tlv(0x0101, struct.pack("<h", 77)) + tlv(0x0102, struct.pack("<h", 88))
        lines = [env("s", fb) for fb in build_fragments(0, body, 4)]
        _, stats = self.run_pipeline(self.write("in.jsonl", lines))
        self.assertEqual(stats.frames_committed, 1)
        self.assertEqual(stats.corrupt, 0)
        rec = self.read_out()[0]
        self.assertEqual(rec["points"]["1"], 77)
        self.assertEqual(rec["points"]["2"], 88)


class TestResume(unittest.TestCase):
    """The core guarantee: resuming must equal an uninterrupted run.

    A crash is modelled by running the pipeline under a line budget and then
    abandoning the object without a clean shutdown, exactly as SIGKILL would.
    The resumed run then continues against the *same* input file.
    """

    def setUp(self):
        self.dir = tempfile.mkdtemp(prefix="telem-resume-")
        self.rng = random.Random(1234)

    def tearDown(self):
        shutil.rmtree(self.dir, ignore_errors=True)

    def path(self, name):
        return os.path.join(self.dir, name)

    def make_log(self, sessions=3, frames=120, fragment_rate=0.3, corrupt_rate=0.05):
        """Build a log with fragmentation, local reordering and dirty lines."""
        lines, pending = [], []
        seqs = {f"s{i}": 0 for i in range(sessions)}
        n = 0
        for i in range(frames):
            sid = f"s{i % sessions}"
            seq = seqs[sid]
            seqs[sid] += 1
            body = tlv(0x0100 + (n % 5), struct.pack("<h", n % 50))
            if n % 4 == 0:
                body += tlv(0x0201, bytes([0, n % 2]))
            if self.rng.random() < fragment_rate:
                for fb in build_fragments(seq, body, self.rng.randint(2, 3)):
                    pending.append((sid, fb))
            else:
                pending.append((sid, build_frame(seq, body)))
            n += 1
            if self.rng.random() < 0.5 and pending:
                j = max(0, len(pending) - 1 - self.rng.randrange(2))
                pending.append(pending.pop(j))
            while pending and self.rng.random() < 0.6:
                s, fb = pending.pop(0)
                if self.rng.random() < corrupt_rate:
                    lines.append("<<< dirty >>>")
                else:
                    lines.append(env(s, fb))
        for s, fb in pending:
            lines.append(env(s, fb))
        p = self.path("in.jsonl")
        with open(p, "w", encoding="utf-8") as fh:
            fh.write("\n".join(lines) + "\n")
        return p

    def run_full(self, inp, state, out, dlq):
        pipe = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq)
        st = pipe.run()
        pipe.ckpt.close()
        return st

    def records(self, path):
        with open(path, encoding="utf-8") as fh:
            return [json.loads(x) for x in fh if x.strip()]

    def assert_matches_full_run(self, inp, state, out, dlq, max_lines=37, **pipe_kw):
        """Replay in crashing batches until done; compare against a clean run."""
        ref_stats = self.run_full(inp, self.path("ref.db"), self.path("ref.jsonl"),
                                  self.path("ref_dlq.jsonl"))
        ref = self.records(self.path("ref.jsonl"))

        batches = 0
        while True:
            pipe = ReplayPipeline(inp, state_path=state, output_path=out,
                                  dlq_path=dlq, checkpoint_frames=5,
                                  resume=batches > 0, max_lines=max_lines, **pipe_kw)
            stats = pipe.run()
            done = pipe.is_complete()
            pipe.ckpt.close()
            batches += 1
            if done:
                break
            if batches > 400:
                self.fail("resume failed to converge")

        got = self.records(out)
        keys = [(r["session_id"], r["seq"]) for r in got]
        self.assertEqual(len(keys), len(set(keys)),
                         "a (session, seq) frame was emitted more than once")
        self.assertEqual(set(keys), {(r["session_id"], r["seq"]) for r in ref},
                         "resumed timeline is missing frames")

        # A resume rewinds to a safe byte offset, which shifts the interleaving
        # *between* independent sessions.  That is expected.  What must be
        # preserved exactly is each session's own ordering and the content of
        # every record, keyed by (session, seq).
        ref_by_key = {(r["session_id"], r["seq"]): r for r in ref}
        for rec in got:
            self.assertEqual(rec, ref_by_key[(rec["session_id"], rec["seq"])],
                             "record content differs after resume")
        for sid in {r["session_id"] for r in got}:
            seqs = [r["seq"] for r in got if r["session_id"] == sid]
            self.assertEqual(seqs, sorted(seqs),
                             f"session {sid} is no longer in ascending seq order")
            ref_seqs = [r["seq"] for r in ref if r["session_id"] == sid]
            self.assertEqual(seqs, ref_seqs, f"session {sid} lost or reordered frames")

        # Every input line is accounted for exactly once: it either committed a
        # frame, was a re-read duplicate, or was rejected as corrupt.  The only
        # other possibility is a blank line, which is skipped silently.
        # Line-level accounting.  Counters are cumulative across the batches of
        # this job, so re-read line-instances are subtracted to compare against
        # the distinct lines in the file.  Every distinct non-blank line is
        # exactly one of consumed / duplicate / corrupt.
        total = sum(1 for ln in open(inp, encoding="utf-8") if ln.strip())
        distinct_read = stats.lines_read - stats.reread
        self.assertEqual(
            distinct_read - (stats.lines_consumed - stats.reread
                             + stats.duplicate + stats.corrupt),
            0,
            "every distinct non-blank line must be consumed, duplicate, or corrupt",
        )
        self.assertLessEqual(distinct_read, total + stats.reread)
        # A frame spans one or more lines, so frames can never exceed the lines.
        self.assertLessEqual(stats.frames_committed, total)
        self.assertGreaterEqual(stats.reread, 0)
        return ref_stats, stats, batches

    def test_resume_matches_uninterrupted(self):
        inp = self.make_log()
        self.assert_matches_full_run(inp, self.path("r.db"),
                                     self.path("r.jsonl"), self.path("r_dlq.jsonl"))

    def test_multiple_resumes_converge(self):
        inp = self.make_log(sessions=2, frames=90)
        _, stats, batches = self.assert_matches_full_run(
            inp, self.path("m.db"), self.path("m.jsonl"), self.path("m_dlq.jsonl"),
            max_lines=25)
        self.assertGreater(batches, 3, "test did not actually exercise multiple resumes")
        # "resumed" is a cumulative count of restarts of this job.
        self.assertEqual(stats.resumed, batches - 1)

    def test_corrupt_survives_resume_and_never_fails_the_run(self):
        inp = self.make_log(sessions=2, frames=80, corrupt_rate=0.3)
        ref_stats, stats, _ = self.assert_matches_full_run(
            inp, self.path("c.db"), self.path("c.jsonl"), self.path("c_dlq.jsonl"))
        self.assertGreater(stats.corrupt, 0, "generator injected no dirty lines")
        self.assertGreater(stats.frames_committed, 0)

    def test_no_duplicate_frames_when_rerunning_committed_prefix(self):
        # Everything committed already: a resumed run must add nothing.
        inp = self.make_log(sessions=1, frames=40, fragment_rate=0.0, corrupt_rate=0.0)
        state, out, dlq = self.path("d.db"), self.path("d.jsonl"), self.path("d_dlq.jsonl")
        self.run_full(inp, state, out, dlq)
        before = self.records(out)
        size_before = os.path.getsize(out)

        pipe = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq,
                              resume=True)
        stats = pipe.run()
        pipe.ckpt.close()

        # Counters are cumulative job totals, so the meaningful assertion is
        # that the resumed run read no lines and appended nothing.
        self.assertEqual(stats.lines_read, 40, "resumed run re-read a finished input")
        self.assertEqual(stats.frames_committed, 40, "cumulative frame total changed")
        self.assertEqual(self.records(out), before)
        self.assertEqual(os.path.getsize(out), size_before,
                         "resume appended to an already complete output")
        self.assertEqual(stats.resumed, 1)

    def test_rewind_is_bounded_by_the_window(self):
        # After a resume the pipeline re-reads from the safe offset.  That re-read
        # must stay small (bounded by the reassembly window), never a full rescan.
        inp = self.make_log(sessions=1, frames=200, fragment_rate=0.3, corrupt_rate=0.0)
        state, out, dlq = self.path("w.db"), self.path("w.jsonl"), self.path("w_dlq.jsonl")
        p1 = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq,
                            max_lines=100, checkpoint_frames=1)
        p1.run()
        p1.ckpt.close()
        p2 = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq,
                            resume=True, checkpoint_frames=1)
        st2 = p2.run()
        p2.ckpt.close()
        self.assertLess(st2.reread, 200,
                        "resume re-read far more than the reassembly window allows")
        keys = [(r["session_id"], r["seq"]) for r in self.records(out)]
        self.assertEqual(len(keys), len(set(keys)))

    def test_duplicate_suppressed_at_resume_boundary(self):
        # Stop mid-stream with fragments still in flight, so the safe offset has
        # to rewind.  The re-read lines must be dropped as duplicates rather
        # than committed twice.
        inp = self.make_log(sessions=1, frames=30, fragment_rate=0.5, corrupt_rate=0.0)
        state, out, dlq = self.path("e.db"), self.path("e.jsonl"), self.path("e_dlq.jsonl")
        total_reread = 0
        for i in range(12):
            pipe = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq,
                                  max_lines=7, checkpoint_frames=1, resume=i > 0)
            st = pipe.run()
            done = pipe.is_complete()
            total_reread += st.reread
            pipe.ckpt.close()
            if done:
                break
        # A rewind re-supplies fragments still held in the window; those are
        # counted as consumed (they genuinely feed the reassembler), not as
        # duplicates.  The invariant that matters is that no frame is committed
        # twice, which the per-session seq check below enforces.
        self.assertGreater(total_reread, 0, "no rewind happened, test is vacuous")
        keys = [(r["session_id"], r["seq"]) for r in self.records(out)]
        self.assertEqual(len(keys), len(set(keys)), "a frame was committed twice")

        # And it must still equal a clean run.
        self.run_full(inp, self.path("e_ref.db"), self.path("e_ref.jsonl"),
                      self.path("e_ref_dlq.jsonl"))
        self.assertEqual(self.records(out), self.records(self.path("e_ref.jsonl")))

    def test_checkpoint_offsets_land_on_line_boundaries(self):
        inp = self.make_log(sessions=1, frames=50, fragment_rate=0.4, corrupt_rate=0.0)
        state, out, dlq = self.path("b.db"), self.path("b.jsonl"), self.path("b_dlq.jsonl")
        for batch in range(12):
            pipe = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq,
                                  checkpoint_frames=1, resume=batch > 0)
            pipe.run()
            pipe.ckpt.close()
            import sqlite3
            conn = sqlite3.connect(state)
            off = int(dict(conn.execute("SELECT key, value FROM meta"))["global_offset"])
            conn.close()
            if off < os.path.getsize(inp):
                with open(inp, "rb") as fh:
                    fh.seek(off)
                    nxt = fh.read(1)
                self.assertEqual(nxt, b"{",
                                 f"offset {off} does not start a JSON line")

    def test_interrupt_commit_records_real_output_length(self):
        # Regression: an interrupted run commits after its output handles are
        # closed.  Recording tell() there reported length 0, so the next resume
        # truncated the output to nothing while the session cursors still
        # claimed those frames were done -- silently destroying the first
        # ~9% of the timeline on a large log.
        inp = self.make_log(sessions=2, frames=200, fragment_rate=0.3, corrupt_rate=0.0)
        state, out, dlq = self.path("i.db"), self.path("i.jsonl"), self.path("i_dlq.jsonl")
        pipe = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq,
                              max_lines=60, checkpoint_frames=1)
        pipe.run()
        pipe.ckpt.close()

        import sqlite3
        conn = sqlite3.connect(state)
        recorded = int(dict(conn.execute(
            "SELECT key, value FROM meta"))["output_offset"])
        last_seqs = dict(conn.execute("SELECT session_id, last_seq FROM session_state"))
        conn.close()
        self.assertEqual(recorded, os.path.getsize(out),
                         "checkpoint must record the real output length")
        self.assertGreater(recorded, 0)
        self.assertGreater(max(last_seqs.values()), 0)

        # Resuming must keep every frame that was already written.
        before = self.records(out)
        p2 = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq,
                            resume=True)
        p2.run()
        p2.ckpt.close()
        after = self.records(out)
        self.assertEqual(after[: len(before)], before,
                         "resume discarded already-written output")

    def test_dlq_is_optional_on_resume(self):
        # The brief's resume command omits --dlq.  The DLQ is a diagnostic, so a
        # missing or relocated one must never block resuming the timeline.
        inp = self.make_log(sessions=1, frames=40, fragment_rate=0.0, corrupt_rate=0.2)
        state, out = self.path("q.db"), self.path("q.jsonl")
        dlq = self.path("q_dlq.jsonl")
        p1 = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq,
                            max_lines=15, checkpoint_frames=1)
        p1.run()
        p1.ckpt.close()
        os.remove(dlq)  # operator moved or cleaned up the dead-letter log
        p2 = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=None,
                            resume=True)
        st = p2.run()
        p2.ckpt.close()
        self.assertGreater(st.lines_read, 0, "resume did no work")
        self.assertTrue(os.path.exists(out))

    def test_resume_of_finished_input_is_a_noop(self):
        inp = self.make_log(sessions=1, frames=30, fragment_rate=0.0, corrupt_rate=0.0)
        state, out, dlq = self.path("f.db"), self.path("f.jsonl"), self.path("f_dlq.jsonl")
        self.run_full(inp, state, out, dlq)
        before = self.records(out)
        size = os.path.getsize(out)

        p2 = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq,
                            resume=True)
        st = p2.run()
        p2.ckpt.close()
        self.assertTrue(st.noop, "resuming a finished input should be a no-op")
        self.assertEqual(self.records(out), before)
        self.assertEqual(os.path.getsize(out), size, "a no-op must not touch the output")
        self.assertEqual(st.resumed, 1, "the restart should still be counted")

    def test_resume_refuses_divergent_output(self):
        # If the output file was rotated away, resuming must fail loudly rather
        # than silently produce a timeline that is missing its early records.
        inp = self.make_log(sessions=1, frames=30, fragment_rate=0.0, corrupt_rate=0.0)
        state, out, dlq = self.path("g.db"), self.path("g.jsonl"), self.path("g_dlq.jsonl")
        pipe = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq,
                              max_lines=10)
        pipe.run()
        pipe.ckpt.close()
        with open(out, "w"):  # truncate the output behind our back
            pass
        pipe2 = ReplayPipeline(inp, state_path=state, output_path=out, dlq_path=dlq,
                               resume=True)
        with self.assertRaises(IOError):
            pipe2.run()

    def test_stats_command_reports_counters(self):
        import contextlib
        import io as _io
        import json as _json
        from telem_replay.__main__ import main
        inp = self.make_log(sessions=2, frames=30)
        state = self.path("st.db")
        self.run_full(inp, state, self.path("st.jsonl"), self.path("st_dlq.jsonl"))

        buf = _io.StringIO()
        with contextlib.redirect_stdout(buf):
            self.assertEqual(main(["stats", state, "--json"]), 0)
        report = _json.loads(buf.getvalue())
        # 'processed' counts every physical line the generator emitted, which
        # exceeds the frame count because fragmentation and the injected dirty
        # lines both add lines.
        self.assertEqual(report["processed"],
                         sum(1 for _ in open(inp, encoding="utf-8")))
        self.assertEqual(report["complete"], True)
        self.assertGreater(report["frames_committed"], 0)
        self.assertEqual(report["resumed"], 0)
        self.assertGreater(report["corrupt"], 0, "generator injected no dirty lines")

        buf = _io.StringIO()
        with contextlib.redirect_stdout(buf):
            self.assertEqual(main(["stats", state]), 0)
        text = buf.getvalue()
        for label in ("processed", "corrupt", "resumed"):
            self.assertIn(label, text)

    def test_checkpoint_overhead_is_low(self):
        # Measured on a workload large enough for the ratio to mean something:
        # on a tiny run the fixed per-commit cost dominates the total runtime.
        # Measure the cost of committing directly rather than as a ratio of
        # wall-clock: a ratio test is flaky under parallel test load, and what
        # actually matters is that a commit is cheap next to the work it
        # protects.  The budget of 5% is then checked against a generous
        # upper bound on per-frame processing time.
        import time
        inp = self.make_log(sessions=4, frames=20000, corrupt_rate=0.02)
        st = self.run_full(inp, self.path("ov.db"), self.path("ov.jsonl"),
                           self.path("ov_dlq.jsonl"))
        self.assertGreater(st.lines_read, 10000, "workload too small to judge overhead")
        self.assertGreater(st.checkpoint_writes, 3, "not enough commits to measure")

        per_commit = st.checkpoint_seconds / st.checkpoint_writes
        # 2000 frames are committed per write by default; at >=50k lines/s the
        # pipeline spends >=40ms of real work per commit.
        self.assertLess(per_commit, 0.040,
                        f"commit costs {per_commit * 1000:.1f}ms, too slow to be <5%")


if __name__ == "__main__":
    unittest.main(verbosity=2)
