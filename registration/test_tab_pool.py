import threading
import time
import unittest

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


if __name__ == "__main__":
    unittest.main()
