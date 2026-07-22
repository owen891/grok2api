import unittest

from protocol_auth.xconsole_client.oauth_protocol import (
    _redact_url,
    extract_submit_oauth_action,
    script_sources,
)


class OAuthActionDiscoveryTests(unittest.TestCase):
    def test_redacts_oauth_query_and_fragment_from_debug_urls(self):
        self.assertEqual(
            "https://accounts.x.ai/oauth2/callback",
            _redact_url("https://accounts.x.ai/oauth2/callback?code=secret&state=state#done"),
        )

    def test_extracts_action_nearest_to_consent_export(self):
        unrelated = "a" * 40
        expected = "4f09e49af5a7498fabcbbabc11dfcc71f79c19ab2"
        source = (
            f'createServerReference)("{unrelated}",null,"otherAction");'
            f'createServerReference)("{expected}",null,"submitOAuth2Consent")'
        )

        self.assertEqual(expected, extract_submit_oauth_action(source))

    def test_extracts_current_create_server_reference_shape(self):
        expected = "004050e5c3e2fde2c7cc40922cef1b491a38b821c8"
        source = (
            'let action=(0,module.createServerReference)("'
            f'{expected}",module.callServer,void 0,module.findSourceMapURL,'
            '"submitOAuth2Consent")'
        )

        self.assertEqual(expected, extract_submit_oauth_action(source))

    def test_resolves_script_sources_against_consent_page(self):
        html = '<script src="/_next/static/chunks/app.js"></script><script src="https://cdn.example/x.js"></script>'

        self.assertEqual(
            [
                "https://accounts.x.ai/_next/static/chunks/app.js",
                "https://cdn.example/x.js",
            ],
            script_sources("https://accounts.x.ai/oauth2/consent?state=x", html),
        )

    def test_discovers_route_chunk_from_rsc_flight_data(self):
        html = '<script>self.__next_f.push([1,"/_next/static/chunks/consent-live.js"])</script>'

        self.assertEqual(
            ["https://accounts.x.ai/_next/static/chunks/consent-live.js"],
            script_sources("https://accounts.x.ai/oauth2/consent?state=x", html),
        )


if __name__ == "__main__":
    unittest.main()
