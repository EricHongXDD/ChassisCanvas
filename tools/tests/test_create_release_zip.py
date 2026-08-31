from __future__ import annotations

import tempfile
import unittest
import zipfile
from pathlib import Path

from tools import create_release_zip


class CreateReleaseZipTests(unittest.TestCase):
    def test_archive_uses_unix_modes(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "release"
            (source / "screen").mkdir(parents=True)
            (source / "deploy").mkdir()
            (source / "screen" / "casescreen-edge").write_bytes(b"ELF")
            (source / "deploy" / "install.sh").write_text("#!/bin/sh\n", encoding="utf-8")
            (source / "README.md").write_text("说明\n", encoding="utf-8")
            target = root / "release.zip"

            create_release_zip.build_archive(source, target)

            with zipfile.ZipFile(target) as archive:
                expected = {
                    "screen/casescreen-edge": 0o755,
                    "deploy/install.sh": 0o755,
                    "README.md": 0o644,
                }
                self.assertEqual(set(archive.namelist()), set(expected))
                for name, mode in expected.items():
                    info = archive.getinfo(name)
                    self.assertEqual(info.create_system, 3)
                    self.assertEqual((info.external_attr >> 16) & 0o777, mode)

    def test_rejects_target_inside_source(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary)
            (source / "README.md").write_text("ok\n", encoding="utf-8")
            with self.assertRaises(ValueError):
                create_release_zip.build_archive(source, source / "release.zip")


if __name__ == "__main__":
    unittest.main()
