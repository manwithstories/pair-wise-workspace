"""Allow ``python -m avtimeline``."""

from .cli import main

if __name__ == "__main__":  # pragma: no cover - module entry
    raise SystemExit(main())
