from protocol_auth.xconsole_client.client import XConsoleAuthClient
from protocol_auth.xconsole_client.logging_utils import redact_url


def test_protocol_debug_url_redacts_credentials_and_oauth_parameters():
    value = redact_url(
        "https://user:secret@accounts.x.ai:8443/set-cookie?q=secret-jwt&code=secret-code#secret-fragment"
    )

    assert value == "https://accounts.x.ai:8443/set-cookie"
    assert "secret" not in value


def test_ambiguous_success_response_does_not_raise_name_error():
    assert XConsoleAuthClient._signup_response_looks_ok(
        'unexpected-rsc-shape', [], {"content-type": "text/x-component"}
    ) is True


def test_explicit_signup_error_is_rejected():
    assert XConsoleAuthClient._signup_response_looks_ok(
        '0:E{"digest":"account_already_exists"}', [], {}
    ) is False
