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


def test_yyds_jwt_can_create_mailbox_without_api_key(monkeypatch):
    session = _Session()
    monkeypatch.setattr(mail, "_create_session", lambda conf: session)
    provider = mail.YydsMailProvider(
        {"id": "jwt-only", "api_base": "https://mail.test/v1", "jwt": "jwt-value", "domain": ["example.com"]},
        mail._config({"providers": []}),
    )

    mailbox = provider.create_mailbox()

    assert mailbox["address"] == "test@example.com"
    assert mailbox["token"] == "mail-token"
    assert session.calls[0][2]["headers"]["Authorization"] == "Bearer jwt-value"
    assert "X-API-Key" not in session.calls[0][2]["headers"]


def test_create_mailbox_keeps_provider_initialization_error(monkeypatch):
    config = {"providers": [{"type": "yyds_mail", "id": "missing-auth", "enable": True}]}

    try:
        mail.create_mailbox(config)
    except RuntimeError as exc:
        assert "api_key" in str(exc) or "jwt" in str(exc)
    else:
        raise AssertionError("expected provider initialization failure")
