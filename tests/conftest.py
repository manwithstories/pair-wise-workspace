import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import make_fixtures  # noqa: E402
from mkvkit import cli  # noqa: E402


@pytest.fixture(scope="session")
def fixtures_dir(tmp_path_factory):
    out = tmp_path_factory.mktemp("fixtures")
    make_fixtures.build_all(out, seed=7)
    return out


@pytest.fixture(scope="session")
def work_dir(tmp_path_factory):
    return tmp_path_factory.mktemp("work")


def run_cli(*argv):
    return cli.main(list(argv))


def read_json(path):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)
