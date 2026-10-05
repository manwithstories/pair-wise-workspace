import os
import sys

# dulwich is a test-only oracle; make it importable from the vendored dir.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".pylibs"))
sys.path.insert(0, os.path.dirname(__file__))
