import pytest

from media_probe import fixtures_gen


@pytest.fixture(scope="session")
def fixtures_dir(tmp_path_factory):
    """Generate the full fixture set once per test session."""
    directory = tmp_path_factory.mktemp("fixtures")
    fixtures_gen.generate_all(str(directory))
    return str(directory)


@pytest.fixture(scope="session")
def drift_path(fixtures_dir):
    import os

    return os.path.join(fixtures_dir, "bframes_drift.mp4")
