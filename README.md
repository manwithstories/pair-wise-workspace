# mediasync

Deterministic multi-track A/V ingestion, recovery, synchronization and
manifest reporting for raw elementary streams, built for macOS terminals.

```
brew install ffmpeg libsndfile
python3.12 -m venv .venv
.venv/bin/python -m pip install -e '.[test]'
.venv/bin/python -m mediasync.cli demo --seed 17 --loss 0.08 --jitter-ms 45 \
    --target-bps 4000000 --out /tmp/mediasync.json
.venv/bin/python -m mediasync.cli verify /tmp/mediasync.json
.venv/bin/python -m pytest -q
```

Exit codes: `0` pass, `2` invalid input/parameters, `3` unrecoverable
intervals present, `4` manifest or synchronization quality gate failed.
