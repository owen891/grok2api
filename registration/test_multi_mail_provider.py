import unittest
from unittest.mock import patch

import multi_mail_provider as mail


class _Response:
    status_code = 201
    text = ""

    def json(self):
        return {"data": {"address": "test@example.com", "token": "mail-token"}}


class _Session:
    def __init__(self):
        self.headers = {}
        self.calls = []

    def request(self, method, url, **kwargs):
        self.calls.append((method, url, kwargs))
        return _Response()

    def close(self):
        pass


class YydsMailProviderTests(unittest.TestCase):
    def test_yyds_jwt_can_create_mailbox_without_api_key(self):
        session = _Session()
        with patch.object(mail, "_create_session", return_value=session):
            provider = mail.YydsMailProvider(
                {"id": "jwt-only", "api_base": "https://mail.test/v1", "jwt": "jwt-value", "domain": ["example.com"]},
                mail._config({"providers": []}),
            )

        mailbox = provider.create_mailbox()

        self.assertEqual("test@example.com", mailbox["address"])
        self.assertEqual("mail-token", mailbox["token"])
        self.assertEqual("Bearer jwt-value", session.calls[0][2]["headers"]["Authorization"])
        self.assertNotIn("X-API-Key", session.calls[0][2]["headers"])

    def test_create_mailbox_keeps_provider_initialization_error(self):
        config = {"providers": [{"type": "yyds_mail", "id": "missing-auth", "enable": True}]}

        with self.assertRaisesRegex(RuntimeError, "api_key|jwt"):
            mail.create_mailbox(config)
