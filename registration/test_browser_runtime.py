import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))

import browser_runtime


class BrowserRuntimeTests(unittest.TestCase):
    def test_background_mode_hides_windows_for_windows_only(self):
        with patch.object(browser_runtime.sys, "platform", "win32"), patch.dict(
            os.environ, {"REGISTRATION_BROWSER_MODE": "background"}
        ), patch("ctypes.windll", create=True):
            self.assertEqual(0, browser_runtime.hide_browser_windows(None))

    def test_browser_path_does_not_fall_back_to_edge(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            edge = root / "Microsoft" / "Edge" / "Application" / "msedge.exe"
            edge.parent.mkdir(parents=True)
            edge.write_text("", encoding="utf-8")

            with (
                patch.dict(
                    os.environ,
                    {
                        "PROGRAMFILES": str(root),
                        "PROGRAMFILES(X86)": "",
                        "LOCALAPPDATA": "",
                        "REGISTRATION_BROWSER_PATH": "",
                    },
                    clear=False,
                ),
                patch("shutil.which", return_value=None),
            ):
                self.assertIsNone(browser_runtime.browser_path())


if __name__ == "__main__":
    unittest.main()
