"""One-shot TLS server for the tls_name_probe.py tooling test.

Usage: tls_probe_server.py <protocol> <cert> <key> <portfile>
  protocol  tls | nats | postgres, as for tls_name_probe.py
Listens on a free loopback port (written to <portfile>), serves connections
until killed, and presents <cert>. It requests no client certificate.
"""

import socket
import ssl
import sys
from pathlib import Path

POSTGRES_SSL_REQUEST_LEN = 8  # bytes in the client's SSLRequest

protocol, cert, key, portfile = sys.argv[1:]
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
srv = socket.socket()
srv.bind(("127.0.0.1", 0))
srv.listen(8)
Path(portfile).write_text(str(srv.getsockname()[1]))
while True:
    conn, _ = srv.accept()
    try:
        if protocol == "nats":
            conn.sendall(b"INFO {}\r\n")
        elif protocol == "postgres":
            conn.recv(POSTGRES_SSL_REQUEST_LEN)
            conn.sendall(b"S")
        ctx.wrap_socket(conn, server_side=True).close()
    except OSError:  # ssl.SSLError too: a failed handshake must not stop the server
        conn.close()
