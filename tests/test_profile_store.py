import pytest

from audiocut import profile
from audiocut.detector import Segment
from audiocut.errors import ProfileError
from audiocut.store import Store


def make_profile(**kw):
    return profile.create_profile(48000, 20.0, 10.0, -66.5, **kw)


def test_profile_roundtrip(tmp_path):
    p = tmp_path / "prof.json"
    profile.save_profile(make_profile(), p)
    loaded = profile.load_profile(p)
    assert loaded["sample_rate"] == 48000
    assert loaded["checksum"]


def test_profile_tamper_rejected(tmp_path):
    p = tmp_path / "prof.json"
    profile.save_profile(make_profile(), p)
    import json
    data = json.loads(p.read_text())
    data["min_silence_ms"] = 999.0
    p.write_text(json.dumps(data))
    with pytest.raises(ProfileError) as exc:
        profile.load_profile(p)
    assert exc.value.exit_code == 4


def test_profile_missing_file(tmp_path):
    with pytest.raises(ProfileError):
        profile.load_profile(tmp_path / "nope.json")


def _info():
    return {"sample_rate": 48000, "channels": 1, "total_samples": 48000}


def test_store_record_and_idempotent(tmp_path):
    store = Store(tmp_path / "i.sqlite")
    segs = [Segment(0, 1000), Segment(2000, 3000)]
    sils = [Segment(1000, 2000), Segment(3000, 48000)]
    store.record_result("a.wav", "digest1", _info(), segs, sils)
    assert store.known_digest("a.wav") == "digest1"
    store.record_result("a.wav", "digest1", _info(), segs, sils)  # rescan same
    rows = store.files()
    assert len(rows) == 1
    assert store.intervals("segments", rows[0][0]) == [(0, 1000), (2000, 3000)]
    assert store.intervals("silences", rows[0][0]) == [(1000, 2000), (3000, 48000)]
    store.close()


def test_store_segments_ordered_and_replaced(tmp_path):
    store = Store(tmp_path / "i.sqlite")
    store.record_result("b.wav", "d1", _info(), [Segment(0, 10)], [])
    store.record_result("b.wav", "d2", _info(),
                        [Segment(5, 50), Segment(60, 70)], [Segment(0, 5)])
    fid = store.files()[0][0]
    assert store.intervals("segments", fid) == [(5, 50), (60, 70)]
    store.close()


def test_store_failure_and_rollback(tmp_path):
    store = Store(tmp_path / "i.sqlite")
    store.record_failure("c.wav", "d1", _info(), "boom")
    row = store.files(ok_only=False)[0]
    assert row[4] == "failed" and store.known_digest("c.wav") is None
    with pytest.raises(RuntimeError):
        with store.conn:  # simulated mid-transaction interrupt
            store.conn.execute(
                "INSERT INTO files (path, sha256, sample_rate, channels,"
                " total_samples, status, scanned_at) VALUES ('x.wav','d',1,1,1,'ok',0)")
            raise RuntimeError("interrupt")
    assert store.conn.execute(
        "SELECT COUNT(*) FROM files WHERE path='x.wav'").fetchone()[0] == 0
    store.close()


def test_runs_table(tmp_path):
    store = Store(tmp_path / "i.sqlite")
    rid = store.begin_run()
    store.finish_run(rid, ok=3, failed=1, status="partial")
    row = store.conn.execute("SELECT files_ok, files_failed, status FROM runs").fetchone()
    assert row == (3, 1, "partial")
    store.close()
