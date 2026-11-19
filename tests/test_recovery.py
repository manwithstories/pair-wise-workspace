"""Edge-case and recovery tests for the archive_restore pipeline."""

from __future__ import annotations

import json
import os
import struct
import tempfile
import unittest

from archive_restore import checkpoint, fixtures, rtp
from archive_restore.index import load_index_tolerant, validate_entries
from archive_restore.model import MEDIA_CLOCK_RATE, Stage
from archive_restore.pipeline import (
    EXIT_CHECKPOINT,
    EXIT_INPUT,
    EXIT_OK,
    run_restore,
    run_verify,
)
from archive_restore.repair import repair_packets


def frame(packet: bytes) -> bytes:
    return struct.pack(">H", len(packet)) + packet


def rtp_bytes(seq: int, ts: int, ssrc: int = fixtures.SSRC,
              payload: bytes = b"\x00" * 320) -> bytes:
    return fixtures.make_rtp_packet(seq, ts, payload, ssrc=ssrc)


class ParseTests(unittest.TestCase):
    def test_bad_length_located_exactly_and_resyncs(self):
        good1 = frame(rtp_bytes(1, 1800))
        good2 = frame(rtp_bytes(2, 3600))
        bad = bytearray(frame(rtp_bytes(3, 5400)))
        struct.pack_into(">H", bad, 0, 8)  # corrupt length field
        stream = good1 + bytes(bad) + good2
        res = rtp.parse_stream(stream)
        self.assertEqual(len(res.errors), 1)
        err = res.errors[0]
        self.assertEqual(err.reason, "bad_length_field")
        self.assertEqual(err.offset_start, len(good1))  # exact byte
        # The corrupted record's payload is unrecoverable (its true
        # length is unknown); parsing resumes at the next record.
        self.assertEqual([p.sequence for p in res.packets], [1, 2])

    def test_truncated_tail_located(self):
        good = frame(rtp_bytes(1, 1800))
        tail = frame(rtp_bytes(2, 3600))[:-10]
        res = rtp.parse_stream(good + tail)
        self.assertEqual(len(res.errors), 1)
        err = res.errors[0]
        self.assertEqual(err.reason, "truncated_tail")
        self.assertEqual(err.offset_start, len(good))
        self.assertEqual(err.offset_end, len(good) + len(tail))

    def test_bad_version_located_at_packet_byte(self):
        good = frame(rtp_bytes(1, 1800))
        bad = bytearray(frame(rtp_bytes(2, 3600)))
        bad[2] = 0x40  # version 1, first byte of the RTP packet
        res = rtp.parse_stream(good + bytes(bad))
        self.assertEqual(len(res.errors), 1)
        self.assertEqual(res.errors[0].reason, "bad_version")
        self.assertEqual(res.errors[0].offset_start, len(good) + 2)
        self.assertEqual([p.sequence for p in res.packets], [1])

    def test_rtcp_sender_report_parsed(self):
        sr = frame(fixtures.make_rtcp_sr(fixtures.SSRC, 1_700_000_000.5, 90000))
        res = rtp.parse_stream(sr + frame(rtp_bytes(1, 1800)))
        self.assertEqual(len(res.sender_reports), 1)
        self.assertAlmostEqual(res.sender_reports[0].ntp_seconds,
                               1_700_000_000.5 + fixtures.NTP_UNIX_EPOCH_OFFSET,
                               places=3)
        self.assertEqual(res.sender_reports[0].rtp_timestamp, 90000)

    def test_extension_and_padding(self):
        payload = b"\x01" * 40
        pad = 8
        pkt = struct.pack(">BBHII", 0xB0, 96, 7, 1234, 55)  # X=1, P=1
        pkt += struct.pack(">HH", 0xBEDE, 1) + b"\x00" * 4  # extension
        pkt += payload + b"\x00" * (pad - 1) + bytes([pad])
        res = rtp.parse_stream(frame(pkt))
        self.assertEqual(len(res.errors), 0)
        self.assertEqual(res.packets[0].payload_length, len(payload))


class RepairTests(unittest.TestCase):
    def _packets(self, seqs, ts_start=0, ts_step=1800):
        data = b"".join(
            frame(rtp_bytes(s, ts_start + i * ts_step)) for i, s in enumerate(seqs)
        )
        return rtp.parse_stream(data).packets

    def test_sequence_wraparound_16bit(self):
        pkts = self._packets([65534, 65535, 0, 1, 2])
        rep = repair_packets(pkts, window=8)
        got = [pkts[i].sequence for i in rep.order]
        self.assertEqual(got, [65534, 65535, 0, 1, 2])
        self.assertEqual(rep.lost, 0)

    def test_reorder_within_window(self):
        pkts = self._packets([10, 12, 11, 13])
        rep = repair_packets(pkts, window=4)
        got = [pkts[i].sequence for i in rep.order]
        self.assertEqual(got, [10, 11, 12, 13])
        self.assertEqual(rep.reordered, 1)

    def test_out_of_window_packet_rejected(self):
        # seq 1 arrives after the window (size 4) has slid past it.
        pkts = self._packets([10, 11, 12, 13, 14, 1])
        rep = repair_packets(pkts, window=4)
        self.assertEqual(rep.out_of_window, 1)
        self.assertTrue(any(e.reason == "out_of_window" for e in rep.errors))
        self.assertNotIn(1, [pkts[i].sequence for i in rep.order])

    def test_duplicate_dropped(self):
        # A true duplicate retransmits identical seq and timestamp.
        data = b"".join([
            frame(rtp_bytes(5, 1000)),
            frame(rtp_bytes(6, 2800)),
            frame(rtp_bytes(6, 2800)),
            frame(rtp_bytes(7, 4600)),
        ])
        pkts = rtp.parse_stream(data).packets
        rep = repair_packets(pkts, window=8)
        self.assertEqual(rep.duplicates, 1)
        self.assertEqual(len(rep.order), 3)

    def test_loss_counted(self):
        pkts = self._packets([20, 21, 24, 25])
        rep = repair_packets(pkts, window=8)
        self.assertEqual(rep.lost, 2)

    def test_timestamp_wrap_32bit(self):
        base = 0xFFFFFFFF - 1800
        pkts = self._packets([1, 2, 3], ts_start=base)
        rep = repair_packets(pkts, window=8)
        ts = [rep.ext_ts[i] for i in rep.order]
        self.assertEqual(ts, sorted(ts))
        self.assertEqual(ts[-1] - ts[0], 2 * 1800)

    def test_timestamp_jump_marks_uncertain(self):
        data = b"".join([
            frame(rtp_bytes(1, 1000)),
            frame(rtp_bytes(2, 2800)),
            frame(rtp_bytes(3, 1000 + MEDIA_CLOCK_RATE * 60)),  # 60 s jump
        ])
        pkts = rtp.parse_stream(data).packets
        rep = repair_packets(pkts, window=8)
        self.assertEqual(len(rep.uncertain), 1)
        self.assertIn("timestamp_jump", rep.uncertain[0].reason)


class IndexTests(unittest.TestCase):
    def test_half_written_index_salvaged(self):
        with tempfile.TemporaryDirectory() as d:
            meta = fixtures.write_fixtures(d, n_packets=300)
            entries, damaged = load_index_tolerant(meta["files"]["index"])
            self.assertTrue(damaged)
            self.assertGreater(len(entries), 0)
            data = open(meta["files"]["clean"], "rb").read()
            valid, failed = validate_entries(entries, data)
            self.assertGreater(len(valid), 0)

    def test_validate_detects_bad_offset(self):
        with tempfile.TemporaryDirectory() as d:
            meta = fixtures.write_fixtures(d, n_packets=100)
            data = open(meta["files"]["clean"], "rb").read()
            entries = [{"ssrc": fixtures.SSRC, "ext_seq": 65000,
                        "timestamp": 0, "time_seconds": 0.0,
                        "offset": 99999999, "length": 332}]
            valid, failed = validate_entries(entries, data)
            self.assertEqual(len(valid), 0)
            self.assertEqual(failed[0][1], "offset_out_of_range")


class PipelineTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = self.tmp.name
        meta = fixtures.write_fixtures(self.dir, n_packets=2000)
        self.input = meta["files"]["damaged"]
        self.index = meta["files"]["index"]
        self.state = os.path.join(self.dir, "state")
        self.damages = meta["damages"]

    def tearDown(self):
        self.tmp.cleanup()

    def _restore(self, **kw):
        args = dict(input_path=self.input, index_path=self.index,
                    state_dir=self.state, window=64)
        args.update(kw)
        return run_restore(**args)

    def test_restore_summary_and_exit_ok(self):
        summary, code = self._restore()
        self.assertEqual(code, EXIT_OK, summary.get("error"))
        self.assertGreater(summary["packets_reordered"], 0)
        self.assertGreater(summary["packets_lost"], 0)
        self.assertEqual(len(summary["output_sha256"]), 64)
        int(summary["output_sha256"], 16)
        self.assertFalse(summary["resumed"])
        # Damage located within 1 byte of where fixtures put it.
        first = summary["first_error_offset"]
        expected = self.damages["bad_length"]["offset"]
        self.assertLessEqual(abs(first - expected), 1)
        reasons = {e["reason"] for e in summary["errors"]}
        self.assertIn("truncated_tail", reasons)
        self.assertIn("bad_version", reasons)
        self.assertIn("bad_length_field", reasons)

    def test_resume_is_deterministic(self):
        s1, code1 = self._restore()
        s2, code2 = self._restore()
        self.assertEqual((code1, code2), (EXIT_OK, EXIT_OK))
        self.assertTrue(s2["resumed"])
        self.assertEqual(s1["output_sha256"], s2["output_sha256"])

    def test_interrupted_resume_reuses_early_stages(self):
        s1, _ = self._restore()
        # Simulate a crash after the repair stage.
        for stage in ("index", "verify"):
            path = checkpoint.stage_path(self.state, stage)
            if os.path.exists(path):
                os.unlink(path)
        manifest = checkpoint.load_manifest(self.state)
        for stage in ("index", "verify"):
            manifest["stages"].pop(stage, None)
        checkpoint._atomic_write_json(checkpoint.manifest_path(self.state),
                                      manifest)
        s2, code = self._restore()
        self.assertEqual(code, EXIT_OK)
        self.assertTrue(s2["stages_cached"]["parse"])
        self.assertTrue(s2["stages_cached"]["repair"])
        self.assertFalse(s2["stages_cached"]["index"])
        self.assertEqual(s1["output_sha256"], s2["output_sha256"])

    def test_input_change_rejected(self):
        self._restore()
        with open(self.input, "r+b") as fh:
            fh.seek(100)
            fh.write(b"\xff")
        summary, code = self._restore()
        self.assertEqual(code, EXIT_CHECKPOINT)
        self.assertIn("input changed", summary["error"])

    def test_param_change_rejected(self):
        self._restore()
        summary, code = self._restore(window=32)
        self.assertEqual(code, EXIT_CHECKPOINT)
        self.assertIn("parameters changed", summary["error"])

    def test_missing_input_is_exit_2(self):
        _s, code = run_restore("/nonexistent.rtp", self.index, self.state)
        self.assertEqual(code, EXIT_INPUT)

    def test_verify_rebuilt_index_ok(self):
        self._restore()
        summary, code = run_verify(self.input, self.index)
        self.assertEqual(code, EXIT_OK)
        self.assertEqual(summary["failed_entries"], 0)
        self.assertGreater(summary["valid_entries"], 0)

    def test_verify_half_written_index_reports_failures(self):
        summary, code = run_verify(self.input, self.index)
        self.assertEqual(code, EXIT_INPUT)
        self.assertTrue(summary["index_damaged"])
        self.assertGreater(summary["failed_entries"], 0)


if __name__ == "__main__":
    unittest.main()
