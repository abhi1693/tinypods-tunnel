import unittest
from contextlib import nullcontext
from unittest.mock import Mock, patch
from uuid import UUID

from locust.env import Environment

from locustfile import TunnelUser


class TunnelWorkloadTests(unittest.TestCase):
    def user_at(self, url):
        with patch.object(TunnelUser, "host", url):
            return TunnelUser(Environment())

    def test_connection_target_and_host_header(self):
        cases = [
            ("http://demo.localhost:8080", "http://localhost:8080", {"Host": "demo.localhost:8080"}),
            ("http://demo.localhost", "http://localhost:80", {"Host": "demo.localhost"}),
            ("http://127.0.0.1:3000", "http://127.0.0.1:3000", None),
            ("https://demo.localhost", "https://demo.localhost", None),
        ]
        for target, connection, headers in cases:
            with self.subTest(target=target):
                user = self.user_at(target)
                self.assertEqual(user.client.base_url, connection)
                self.assertEqual(user.default_headers, headers)

    def test_health_requires_echoed_id_and_success_status(self):
        user = self.user_at("http://localhost:3000")
        request_id = UUID(int=1)
        for status, content, valid in [
            (200, request_id.hex.encode(), True),
            (200, b"another request", False),
            (503, request_id.hex.encode(), False),
        ]:
            with self.subTest(status=status, content=content):
                reply = Mock(status_code=status, content=content)
                with patch.object(user.client, "get", return_value=nullcontext(reply)) as get:
                    with patch("locustfile.uuid4", return_value=request_id):
                        user.health()
                get.assert_called_once_with(
                    f"/health?id={request_id.hex}", name="/health", allow_redirects=False, catch_response=True
                )
                if valid:
                    reply.failure.assert_not_called()
                else:
                    reply.failure.assert_called_once_with(f"Unexpected health response (HTTP {status})")

    def test_webhook_checks_status_delivery_id_and_binary_body(self):
        user = self.user_at("http://localhost:3000")
        delivery = UUID(int=2)
        payload = delivery.bytes + bytes(range(256)) * 4
        mismatch = "Webhook delivery ID or body changed"
        for status, returned_id, body, error in [
            (202, delivery.hex, payload, None),
            (200, delivery.hex, payload, "Unexpected webhook status (HTTP 200)"),
            (202, "other", payload, mismatch),
            (202, delivery.hex, payload[:-1], mismatch),
        ]:
            with self.subTest(status=status, returned_id=returned_id, error=error):
                reply = Mock(status_code=status, headers={"X-Delivery-ID": returned_id}, content=body)
                with patch.object(user.client, "post", return_value=nullcontext(reply)) as post:
                    with patch("locustfile.uuid4", return_value=delivery):
                        user.webhook()
                post.assert_called_once_with(
                    f"/webhook?id={delivery.hex}",
                    name="/webhook",
                    headers={"Content-Type": "application/octet-stream", "X-Delivery-ID": delivery.hex},
                    data=payload,
                    allow_redirects=False,
                    catch_response=True,
                )
                if error:
                    reply.failure.assert_called_once_with(error)
                else:
                    reply.failure.assert_not_called()
