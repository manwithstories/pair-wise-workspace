from __future__ import annotations

from pathlib import Path

import pytest

from make_fixtures import generate
from mkvmp4.cli import main
from mkvmp4.ebml.reader import EBMLDecodeError, EBMLReader
from mkvmp4.probe import parse_digest, probe_file


@pytest.fixture(scope="session")
def sample_dir(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("samples")
    generate(root, 4)
    return root


def test_unknown_size_summary_and_digest(sample_dir: Path) -> None:
    path = sample_dir / "case_b_restart.mkv"
    result = probe_file(str(path), max_depth=4)
    assert result.elements == 613
    assert result.unknown_size == 7
    assert result.clusters == 5
    assert result.tracks == 4
    assert result.chapters == 6
    assert result.digest == parse_digest(str(path))
    assert result.tree[0]["name"] == "EBML"
    assert result.tree[1]["name"] == "Segment"


def test_crc_corruption_is_rejected(sample_dir: Path, tmp_path: Path) -> None:
    data = bytearray((sample_dir / "case_c_lace.mkv").read_bytes())
    data[300] ^= 1
    corrupt = tmp_path / "corrupt.mkv"
    corrupt.write_bytes(data)
    with pytest.raises(EBMLDecodeError, match="CRC-32"):
        with EBMLReader(corrupt) as reader:
            reader.walk()


def test_cli_bad_input_returns_two(sample_dir: Path, tmp_path: Path,
                                   capsys: pytest.CaptureFixture[str]) -> None:
    bad = tmp_path / "truncated.mkv"
    bad.write_bytes(b"\x1a\x45\xdf")
    assert main(["probe", str(bad)]) == 2
    assert "input error" in capsys.readouterr().err

