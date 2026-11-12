"""Allow ``python -m taint``."""

from .cli import main


raise SystemExit(main())
