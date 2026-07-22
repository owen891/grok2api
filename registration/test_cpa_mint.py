import unittest
from pathlib import Path
from unittest.mock import patch

from log_safety import redact_url
from cpa_xai.mint import mint_and_export

TOKENS = {
    "access_token": "access-token",
    "refresh_token": "refresh-token",
    "expires_in": 3600,
}


class CPAMintTests(unittest.TestCase):
    def test_browser_log_url_redacts_device_and_oauth_parameters(self):
        value = redact_url("https://user:secret@auth.x.ai/oauth2/device?user_code=secret-code#secret")

        self.assertEqual("https://auth.x.ai/oauth2/device", value)
        self.assertNotIn("secret", value)

    @patch("cpa_xai.mint.write_cpa_xai_auth", return_value=Path("xai-user@example.com.json"))
    @patch(
        "cpa_xai.mint.probe_mini_response",
        return_value={"ok": False, "status": 403, "error": "permission-denied"},
    )
    @patch(
        "cpa_xai.mint.probe_models",
        return_value={"ok": True, "has_grok_45": True, "model_ids": ["grok-4.5"]},
    )
    @patch("cpa_xai.mint.mint_with_browser", return_value=TOKENS)
    def test_permission_denied_chat_probe_blocks_automatic_import(
        self, _mint, _models, _chat, _write
    ):
        result = mint_and_export(
            email="user@example.com",
            password="password",
            auth_dir="auths",
        )

        self.assertTrue(result["ok"])
        self.assertNotIn("error", result)
        self.assertEqual("permission-denied", result["probe_chat"]["error"])
        self.assertEqual("cpa_chat_probe_failed", result["warnings"][0]["code"])
        self.assertIn("permission-denied", result["warnings"][0]["message"])
        self.assertFalse(result["importable"])
        self.assertEqual("cpa_chat_permission_denied", result["import_block_reason"])

    @patch("cpa_xai.mint.write_cpa_xai_auth", return_value=Path("xai-user@example.com.json"))
    @patch(
        "cpa_xai.mint.probe_mini_response",
        return_value={
            "ok": False,
            "status": 403,
            "error": "Access to the chat endpoint is denied; log into console.x.ai and update the permissions",
        },
    )
    @patch(
        "cpa_xai.mint.probe_models",
        return_value={"ok": True, "has_grok_45": True, "model_ids": ["grok-4.5"]},
    )
    @patch("cpa_xai.mint.mint_with_browser", return_value=TOKENS)
    def test_console_permission_message_blocks_automatic_import(
        self, _mint, _models, _chat, _write
    ):
        result = mint_and_export(
            email="user@example.com",
            password="password",
            auth_dir="auths",
        )

        self.assertFalse(result["importable"])
        self.assertEqual("cpa_chat_permission_denied", result["import_block_reason"])

    @patch("cpa_xai.mint.write_cpa_xai_auth", return_value=Path("xai-user@example.com.json"))
    @patch(
        "cpa_xai.mint.probe_models",
        return_value={"ok": True, "has_grok_45": False, "model_ids": []},
    )
    @patch("cpa_xai.mint.mint_with_browser", return_value=TOKENS)
    def test_missing_required_model_still_blocks_import(self, _mint, _models, _write):
        result = mint_and_export(
            email="user@example.com",
            password="password",
            auth_dir="auths",
        )

        self.assertFalse(result["ok"])
        self.assertEqual("token ok but grok-4.5 not listed", result["error"])


if __name__ == "__main__":
    unittest.main()
