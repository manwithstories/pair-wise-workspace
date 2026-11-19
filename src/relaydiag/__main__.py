"""Enable ``python -m relaydiag`` as the documented entry point."""

from .cli import main

if __name__ == "__main__":
    raise SystemExit(main())
