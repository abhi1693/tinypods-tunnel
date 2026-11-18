"""Exercise the local fixture directly or through the TinyPods relay."""

from urllib.parse import urlsplit
from uuid import uuid4

from locust import FastHttpUser, task


class TunnelUser(FastHttpUser):
    connection_timeout = 5
    network_timeout = 10
    insecure = False
    max_retries = 0

    def __init__(self, environment):
        target = urlsplit(self.host or "")
        if target.scheme == "http" and (target.hostname or "").endswith(".localhost"):
            # Systems vary in how they resolve *.localhost. Dial loopback while
            # preserving the printed authority that selects the tunnel.
            self.default_headers = {"Host": target.netloc}
            self.host = target._replace(netloc=f"localhost:{target.port or 80}").geturl()
        super().__init__(environment)

    @task
    def health(self):
        request_id = uuid4().hex
        with self.client.get(
            f"/health?id={request_id}",
            name="/health",
            allow_redirects=False,
            catch_response=True,
        ) as response:
            if response.status_code != 200 or response.content != request_id.encode():
                response.failure(f"Unexpected health response (HTTP {response.status_code})")

    @task
    def webhook(self):
        delivery = uuid4()
        payload = delivery.bytes + bytes(range(256)) * 4
        with self.client.post(
            f"/webhook?id={delivery.hex}",
            name="/webhook",
            headers={"Content-Type": "application/octet-stream", "X-Delivery-ID": delivery.hex},
            data=payload,
            allow_redirects=False,
            catch_response=True,
        ) as response:
            if response.status_code != 202:
                response.failure(f"Unexpected webhook status (HTTP {response.status_code})")
            elif response.headers.get("X-Delivery-ID") != delivery.hex or response.content != payload:
                response.failure("Webhook delivery ID or body changed")
