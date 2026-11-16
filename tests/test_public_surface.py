"""Public-surface smoke tests: every documented module imports and exposes."""

from __future__ import annotations

import importlib
import unittest


PUBLIC_MODULES = [
    "src.ebml_schema",
    "src.ebml_reader",
    "src.matroska_model",
    "src.timestamp",
    "src.track_mapper",
    "src.mp4_boxes",
    "src.index_rebuilder",
    "src.fixture_builder",
    "src.cli",
    "src.remux",
    "src.verify",
]

REQUIRED_SYMBOLS = {
    "src.ebml_schema": ["SCHEMA", "SchemaError", "encode_vint", "decode_vint", "ElementSpec"],
    "src.ebml_reader": ["EbmlReader", "Element", "ReaderError", "ReaderOptions"],
    "src.matroska_model": ["MatroskaDocument", "Track", "Cluster", "Block", "build_document"],
    "src.timestamp": ["plan_rewrite", "TimingConfig", "TimestampError", "monotonic_ok"],
    "src.track_mapper": ["build_track_mapping", "TrackMapping", "TrackMappingError"],
    "src.mp4_boxes": ["Box", "serialize", "parse_boxes", "build_layout", "MovieSpec"],
    "src.index_rebuilder": ["build_track_index", "TrackIndex", "Sample", "lay_out_media"],
    "src.fixture_builder": ["build_fixture", "demo_spec", "minimal_spec", "write_fixture"],
    "src.cli": ["main", "build_parser"],
    "src.remux": ["remux", "RemuxOptions", "RemuxError"],
    "src.verify": ["verify_output", "VerifyReport"],
}


class PublicSurfaceTests(unittest.TestCase):
    def test_all_modules_import(self) -> None:
        for name in PUBLIC_MODULES:
            with self.subTest(module=name):
                self.assertIsNotNone(importlib.import_module(name))

    def test_required_symbols_present(self) -> None:
        for module_name, symbols in REQUIRED_SYMBOLS.items():
            module = importlib.import_module(module_name)
            for symbol in symbols:
                with self.subTest(module=module_name, symbol=symbol):
                    self.assertTrue(hasattr(module, symbol), f"{module_name}.{symbol} missing")

    def test_matches_planned_architecture(self) -> None:
        """The ten planned modules all exist under their documented names."""
        import pathlib

        root = pathlib.Path(__file__).resolve().parent.parent / "src"
        for filename in (
            "ebml_schema.py",
            "ebml_reader.py",
            "matroska_model.py",
            "timestamp.py",
            "track_mapper.py",
            "mp4_boxes.py",
            "index_rebuilder.py",
            "fixture_builder.py",
            "cli.py",
        ):
            with self.subTest(filename=filename):
                self.assertTrue((root / filename).is_file())

    def test_no_external_media_tool_imports(self) -> None:
        """No subprocess/ffmpeg/mkvmerge dependence anywhere in src."""
        import pathlib

        root = pathlib.Path(__file__).resolve().parent.parent / "src"
        for path in root.glob("*.py"):
            text = path.read_text()
            with self.subTest(file=path.name):
                self.assertNotIn("import subprocess", text)
                self.assertNotIn("ffmpeg", text.lower())
                self.assertNotIn("mkvmerge", text.lower())

    def test_source_files_stay_readable(self) -> None:
        """Every module stays under ~400 lines; the planned public ones under 300."""
        import pathlib

        root = pathlib.Path(__file__).resolve().parent.parent / "src"
        planned = {
            "ebml_schema.py",
            "ebml_reader.py",
            "matroska_model.py",
            "timestamp.py",
            "track_mapper.py",
            "mp4_boxes.py",
            "index_rebuilder.py",
            "fixture_builder.py",
            "cli.py",
        }
        for path in root.glob("*.py"):
            length = len(path.read_text().splitlines())
            limit = 300 if path.name in planned else 400
            with self.subTest(file=path.name, lines=length):
                self.assertLessEqual(length, limit, f"{path.name} has {length} lines")

    def test_only_stdlib_and_numpy_imported(self) -> None:
        import ast
        import pathlib
        import sys

        stdlib = set(sys.stdlib_module_names)
        allowed = stdlib | {"numpy", "src"}
        root = pathlib.Path(__file__).resolve().parent.parent / "src"
        for path in root.glob("*.py"):
            tree = ast.parse(path.read_text())
            for node in ast.walk(tree):
                if isinstance(node, ast.Import):
                    names = [alias.name.split(".")[0] for alias in node.names]
                elif isinstance(node, ast.ImportFrom) and node.level == 0 and node.module:
                    names = [node.module.split(".")[0]]
                else:
                    continue
                for name in names:
                    with self.subTest(file=path.name, module=name):
                        self.assertIn(name, allowed)


if __name__ == "__main__":
    unittest.main()
