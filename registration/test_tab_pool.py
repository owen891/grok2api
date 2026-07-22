import sys
import threading
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from tab_pool import TabPool


class TabPoolReleaseTests(unittest.TestCase):
    def tearDown(self):
        TabPool._thread_local.browser = None
        TabPool._thread_local.tab = None
        TabPool._thread_local.served = 0
        with TabPool._all_browsers_lock:
            TabPool._all_browsers.clear()

    def test_timed_release_detaches_before_slow_quit_finishes(self):
        release_quit = threading.Event()

        class SlowBrowser:
            def quit(self, del_data=True):
                release_quit.wait(1)

        browser = SlowBrowser()
        TabPool._thread_local.browser = browser
        TabPool._thread_local.tab = object()
        TabPool._all_browsers = [browser]

        started = time.monotonic()
        completed = TabPool.release_tab(timeout=0.01)

        self.assertFalse(completed)
        self.assertLess(time.monotonic() - started, 0.2)
        self.assertIsNone(TabPool.get_browser())
        self.assertEqual(0, TabPool.live_count())
        release_quit.set()

    def test_browser_creation_hides_background_window(self):
        browser = SimpleNamespace(process_id=4321)
        with (
            patch.dict(sys.modules, {"DrissionPage": SimpleNamespace(Chromium=lambda _options: browser)}),
            patch("browser_runtime.hide_browser_windows") as hide_windows,
        ):
            TabPool.init(lambda: object())
            self.assertIs(TabPool._create_browser(), browser)

        hide_windows.assert_called_once_with(4321)


if __name__ == "__main__":
    unittest.main()
