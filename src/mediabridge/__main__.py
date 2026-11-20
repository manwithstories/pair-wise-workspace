"""Entry point for ``python -m mediabridge`` when the package is installed."""

import sys

from .cli import main

if __name__ == "__main__":
    sys.exit(main())
