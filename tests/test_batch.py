"""Batch tests: discovery, manifests, ordering, failure isolation, exit codes."""

from __future__ import annotations

from pathlib import Path

import pytest

from audio_tool import batch
from audio_tool.model import ProcessConfig, UsageError


def config(**overrides) -> ProcessConfig:
    values = dict(target_rate=48000, target_lufs=-16.0, true_peak_db=-1.0)
    values.update(overrides)
    return ProcessConfig(**values)


def test_discover_inputs_sorts_and_filters(fixture_tree):
    root = fixture_tree["tone_44100_stereo"].parent
    found = batch.discover_inputs(root)
    relatives = [str(path.relative_to(root)) for path in found]
    assert relatives == sorted(relatives)
    assert all(name.endswith((".wav", ".flac")) for name in relatives)
    assert len(found) >= 7


def test_discover_inputs_accepts_a_single_file(fixture_tree):
    single = batch.discover_inputs(fixture_tree["tone_44100_stereo"])
    assert single == [fixture_tree["tone_44100_stereo"]]


def test_discover_inputs_rejects_missing_path(tmp_path):
    with pytest.raises(UsageError):
        batch.discover_inputs(tmp_path / "nope")


def test_discover_inputs_ignores_non_audio(tmp_path):
    (tmp_path / "notes.txt").write_text("hello")
    assert batch.discover_inputs(tmp_path) == []


def test_manifest_resolves_relative_to_itself(tmp_path, fixture_tree):
    sub = tmp_path / "inputs"
    sub.mkdir()
    import shutil

    shutil.copy(fixture_tree["tone_44100_stereo"], sub / "one.wav")
    shutil.copy(fixture_tree["quiet_music"], sub / "two.wav")
    manifest = tmp_path / "list.txt"
    manifest.write_text("# comment\none.wav\n\nsub/../two.wav\n")
    entries = batch.parse_manifest(manifest)
    assert [path.name for path in entries] == ["one.wav", "two.wav"]


def test_manifest_rejects_unsupported_extension(tmp_path):
    manifest = tmp_path / "list.txt"
    manifest.write_text("song.mp3\n")
    with pytest.raises(UsageError):
        batch.parse_manifest(manifest)


def test_manifest_rejects_empty_file(tmp_path):
    manifest = tmp_path / "list.txt"
    manifest.write_text("# nothing here\n")
    with pytest.raises(UsageError):
        batch.parse_manifest(manifest)


def test_collect_inputs_deduplicates(fixture_tree):
    path = fixture_tree["tone_44100_stereo"]
    collected = batch.collect_inputs([path, path], None)
    assert len(collected) == 1


def test_plan_destinations_mirrors_structure(tmp_path):
    inputs = [tmp_path / "in" / "a.wav", tmp_path / "in" / "sub" / "b.wav"]
    plan = batch.plan_destinations(inputs, tmp_path / "out", tmp_path / "in")
    assert plan[0][1] == tmp_path / "out" / "a.wav"
    assert plan[1][1] == tmp_path / "out" / "sub" / "b.wav"


def test_run_batch_succeeds_and_preserves_order(fixture_tree, tmp_path):
    inputs = [fixture_tree["tone_44100_stereo"], fixture_tree["mono_odd_tail"]]
    plan = batch.plan_destinations(inputs, tmp_path / "out", None)
    result = batch.run_batch(plan, config(), jobs=1)
    assert result.exit_code() == 0
    assert [o.relative for o in result.outcomes] == [
        "tone_44100_stereo.wav", "mono_odd_tail.wav"]
    assert all(o.ok for o in result.outcomes)
    assert all(o.destination.exists() for o in result.outcomes)


def test_run_batch_isolates_failures(fixture_tree, tmp_path):
    inputs = [fixture_tree["tone_44100_stereo"],
              fixture_tree["broken_bad_channels"], fixture_tree["quiet_music"],
              fixture_tree["broken_truncated"]]
    plan = batch.plan_destinations(inputs, tmp_path / "out", None)
    result = batch.run_batch(plan, config(), jobs=1)
    assert result.exit_code() == 1
    assert len(result.succeeded) == 2
    assert len(result.failed) == 2
    codes = {o.relative: o.error_code for o in result.failed}
    assert codes["bad_channels.wav"] == "unsupported_format"
    assert codes["truncated.wav"] == "input_invalid"


def test_silent_file_is_processed_not_failed(fixture_tree, tmp_path):
    # Silence has no loudness target, but it is still a valid delivery file:
    # it passes through at unity and the report explains the status.
    out = tmp_path / "out"
    plan = batch.plan_destinations([fixture_tree["silent"]], out, None)
    result = batch.run_batch(plan, config(), jobs=1)
    assert result.exit_code() == 0
    outcome = result.outcomes[0]
    assert outcome.ok
    assert outcome.result.status == "silent"
    assert (out / "silent.wav").exists()


def test_failed_files_leave_no_output(fixture_tree, tmp_path):
    out = tmp_path / "out"
    plan = batch.plan_destinations([fixture_tree["broken_bad_channels"]], out, None)
    result = batch.run_batch(plan, config(), jobs=1)
    assert result.outcomes[0].error_code == "unsupported_format"
    assert not (out / "bad_channels.wav").exists()
    assert not out.exists() or list(out.iterdir()) == []


def test_threaded_run_matches_single_threaded(fixture_tree, tmp_path):
    inputs = [fixture_tree[name] for name in
              ("tone_44100_stereo", "tone_48000_51", "quiet_music")]
    plan = batch.plan_destinations(inputs, tmp_path / "out", None)
    serial = batch.run_batch(plan, config(), jobs=1)
    threaded = batch.run_batch(plan, config(), jobs=3)
    assert [o.relative for o in serial.outcomes] == [
        o.relative for o in threaded.outcomes]
    for a, b in zip(serial.outcomes, threaded.outcomes):
        assert a.ok and b.ok
        assert a.result.after.lufs == pytest.approx(
            b.result.after.lufs, abs=1e-9)


def test_kernel_cache_shares_per_rate(fixture_tree, tmp_path):
    paths = [fixture_tree["tone_44100_stereo"], fixture_tree["tone_96000_quad"]]
    config_obj = config()
    cache = batch.KernelCache(config_obj)
    first = cache.get(44100)
    second = cache.get(44100)
    third = cache.get(96000)
    assert first is second
    assert first is not third
    assert cache.rates == [44100, 96000]
    assert first.signature() == batch.KernelCache(config_obj).get(44100).signature()


def test_progress_callback_sees_every_file(fixture_tree, tmp_path):
    inputs = [fixture_tree["tone_44100_stereo"], fixture_tree["broken_bad_rate"]]
    plan = batch.plan_destinations(inputs, tmp_path / "out", None)
    seen: list[str] = []
    batch.run_batch(plan, config(), jobs=1, progress=lambda o: seen.append(o.relative))
    assert seen == ["tone_44100_stereo.wav", "bad_rate.wav"]


def test_batch_rejects_zero_jobs(fixture_tree, tmp_path):
    plan = batch.plan_destinations([fixture_tree["tone_44100_stereo"]],
                                   tmp_path / "out", None)
    with pytest.raises(UsageError):
        batch.run_batch(plan, config(), jobs=0)


def test_kernel_cache_is_thread_safe():
    import threading

    cache = batch.KernelCache(config())
    built: list[int] = []

    def worker() -> None:
        built.append(id(cache.get(44100)))

    threads = [threading.Thread(target=worker) for _ in range(8)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    assert len(set(built)) == 1, "concurrent workers built more than one kernel"
