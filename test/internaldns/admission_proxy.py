#!/usr/bin/env python3
"""Development TLS front door for independent internal DNS admission replicas.

Relay AdmissionReview bytes unchanged. A transport failure retries another
certificate-verified backend; an admission decision is returned unchanged.
This fixture exercises controller failover, not a production load balancer.
"""

from __future__ import annotations

import argparse
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import socket
import ssl
import threading
import time
from urllib.parse import urlsplit


MAX_BYTES = 1024 * 1024
PATH = "/validate-internal-dns"


def address(value: str) -> tuple[str, int]:
    host, port = value.rsplit(":", 1)
    return host, int(port)


class VerifiedBackend(http.client.HTTPSConnection):
    def __init__(self, backend: tuple[str, int], context: ssl.SSLContext, server_name: str, timeout: float):
        super().__init__(backend[0], backend[1], timeout=timeout, context=context)
        self.server_name = server_name

    def connect(self) -> None:
        raw = socket.create_connection((self.host, self.port), self.timeout)
        try:
            self.sock = self._context.wrap_socket(raw, server_hostname=self.server_name)
        except BaseException:
            raw.close()
            raise


class FrontDoor(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, listen: tuple[str, int], backends: list[tuple[str, int]], context: ssl.SSLContext, server_name: str, timeout: float):
        super().__init__(listen, Handler)
        self.backends = backends
        self.backend_context = context
        self.backend_server_name = server_name
        self.backend_timeout = timeout
        self.selection_lock = threading.Lock()
        self.next_backend = 0
        self.retry_after: dict[tuple[str, int], float] = {}

    def candidates(self) -> list[tuple[str, int]]:
        with self.selection_lock:
            offset = self.next_backend
            self.next_backend = (offset + 1) % len(self.backends)
            ordered = self.backends[offset:] + self.backends[:offset]
            now = time.monotonic()
            healthy = [backend for backend in ordered if self.retry_after.get(backend, 0) <= now]
            return healthy or ordered

    def failed(self, backend: tuple[str, int]) -> None:
        with self.selection_lock:
            self.retry_after[backend] = time.monotonic() + 2


class Handler(BaseHTTPRequestHandler):
    server: FrontDoor

    def do_POST(self) -> None:
        if urlsplit(self.path).path != PATH:
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self.send_error(400, "invalid content length")
            return
        if not 0 < length <= MAX_BYTES:
            self.send_error(413)
            return
        self.connection.settimeout(3)
        try:
            payload = self.rfile.read(length)
            if len(payload) != length:
                self.send_error(400, "incomplete request")
                return
            for backend in self.server.candidates():
                connection = VerifiedBackend(
                    backend, self.server.backend_context,
                    self.server.backend_server_name, self.server.backend_timeout,
                )
                try:
                    connection.request("POST", self.path, payload, {
                        "Content-Type": "application/json", "Connection": "close",
                    })
                    response = connection.getresponse()
                    body = response.read(MAX_BYTES + 1)
                    if len(body) > MAX_BYTES:
                        raise OSError("backend response exceeds fixture bound")
                    self.send_response(response.status)
                    self.send_header("Content-Type", response.getheader("Content-Type", "application/json"))
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                    return
                except (OSError, http.client.HTTPException) as error:
                    self.log_error("backend %s:%s transport failed: %s", backend[0], backend[1], type(error).__name__)
                    self.server.failed(backend)
                finally:
                    connection.close()
            self.send_error(503, "all certificate-verified admission backends unavailable")
        except (OSError, http.client.HTTPException):
            # The API server can cancel its request while a paused backend times
            # out. Never dump AdmissionReview payloads or credentials to logs.
            return


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen", default="0.0.0.0:19443")
    parser.add_argument("--cert", required=True)
    parser.add_argument("--key", required=True)
    parser.add_argument("--ca", required=True)
    parser.add_argument("--backend", action="append", required=True)
    parser.add_argument("--backend-server-name", default="host.docker.internal")
    parser.add_argument("--backend-timeout", type=float, default=0.8)
    args = parser.parse_args()
    if not 0 < args.backend_timeout <= 1:
        parser.error("backend timeout must be greater than zero and at most one second")
    verified = ssl.create_default_context(cafile=args.ca)
    verified.minimum_version = ssl.TLSVersion.TLSv1_2
    server = FrontDoor(address(args.listen), [address(value) for value in args.backend], verified, args.backend_server_name, args.backend_timeout)
    incoming = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    incoming.minimum_version = ssl.TLSVersion.TLSv1_2
    incoming.load_cert_chain(args.cert, args.key)
    server.socket = incoming.wrap_socket(server.socket, server_side=True)
    print(f"admission fixture listening on {args.listen} with {len(args.backend)} verified backends", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
