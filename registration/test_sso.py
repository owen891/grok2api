import json
import os
import stat
import tempfile
import unittest
from pathlib import Path

from protocol_auth.xconsole_client.sso import save_sso


class SavedSSOTests(unittest.TestCase):
    def test_save_sso_writes_credentials_atomically_with_private_permissions(self):
        with tempfile.TemporaryDirectory() as directory:
            output_dir = Path(directory) / "sso_output"
            saved = save_sso("session-token", email="user@example.com", password="secret", output_dir=output_dir)

            record = json.loads(saved.read_text(encoding="utf-8"))
            self.assertEqual("user@example.com", record["email"])
            self.assertEqual("secret", record["password"])
            self.assertEqual("session-token", record["sso"])
            self.assertTrue(record["created_at"])
            self.assertEqual([], list(output_dir.glob(".sso-*.tmp")))
            if os.name != "nt":
                self.assertEqual(0o700, stat.S_IMODE(output_dir.stat().st_mode))
                self.assertEqual(0o600, stat.S_IMODE(saved.stat().st_mode))


if __name__ == "__main__":
    unittest.main()
