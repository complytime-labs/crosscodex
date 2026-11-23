"""One-shot TLS server for the tls_name_probe.py tooling test.

Usage: tls_probe_server.py <protocol> <cert> <key> <portfile>
  protocol  tls | nats | postgres, as for tls_name_probe.py
Listens on a free loopback port (written to <portfile>), serves connections
until killed, and presents <cert>. It requests no client certificate.
"""

import socket
import ssl
import sys

protocol, cert, key, portfile = sys.argv[1:]
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
srv = socket.socket()
srv.bind(("127.0.0.1", 0))
srv.listen(8)
with open(portfile, "w") as f:
    f.write(str(srv.getsockname()[1]))
while True:
    conn, _ = srv.accept()
    try:
        if protocol == "nats":
            conn.sendall(b"INFO {}\r\n")
        elif protocol == "postgres":
            conn.recv(8)
            conn.sendall(b"S")
        ctx.wrap_socket(conn, server_side=True).close()
    except Exception:
        conn.close()
