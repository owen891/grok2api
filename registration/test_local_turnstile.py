import unittest

from local_turnstile import _dockerize_loopback_proxy


class DockerProxyMappingTests(unittest.TestCase):
    def test_maps_host_loopback_for_docker_desktop(self):
        self.assertEqual(
            _dockerize_loopback_proxy("http://127.0.0.1:7897"),
            "http://host.docker.internal:7897",
        )

    def test_preserves_remote_proxy(self):
        self.assertEqual(
            _dockerize_loopback_proxy("socks5://user:pass@proxy.example:1080"),
            "socks5://user:pass@proxy.example:1080",
        )


if __name__ == "__main__":
    unittest.main()
