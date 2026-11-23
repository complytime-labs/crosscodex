"""Complete a TLS handshake with a service and say whether the certificate it
presents is valid for a given hostname.

smoke_up.sh runs this from a sibling container on the compose network to prove
that each service's certificate is rejected for every other service's hostname
(issue #298), and accepted for its own so the probe cannot pass vacuously.

Usage: tls_name_probe.py <protocol> <connect-host> <port> <ca> <cert> <key> <server-name>
  protocol     tls       the service speaks TLS from the first byte
               nats      the service sends a plaintext INFO line, then upgrades
               postgres  the service answers an SSLRequest, then upgrades
  connect-host where to connect (a compose service name or an address)
  server-name  the hostname the certificate must be valid for; verified
               against the certificate, not against connect-host
  cert, key    the client identity to present (mTLS services require one)

Exit status: 0 the handshake completed and the certificate is valid for
server-name; 4 the certificate was refused as not valid for server-name;
2 anything else, so nothing was proven.
"""

import socket
import ssl
import sys

# X509_V_ERR_HOSTNAME_MISMATCH in OpenSSL's x509_vfy.h.
X509_V_ERR_HOSTNAME_MISMATCH = 62
POSTGRES_SSL_REQUEST = b"\x00\x00\x00\x08\x04\xd2\x16\x2f"


def done(rc, msg):
    print(msg)
    sys.exit(rc)


def main():
    if len(sys.argv) != 8:
        done(2, "usage: tls_name_probe.py <protocol> <connect-host> <port> <ca> <cert> <key> <server-name>")
    protocol, host, port, ca, cert, key, name = sys.argv[1:]
    if protocol not in ("tls", "nats", "postgres"):
        done(2, "unknown protocol %r" % protocol)
    try:
        ctx = ssl.create_default_context(cafile=ca)
        ctx.load_cert_chain(cert, key)
        raw = socket.create_connection((host, int(port)), timeout=15)
        if protocol == "nats":
            info = raw.makefile("rb").readline()
            if not info.startswith(b"INFO "):
                done(2, "no NATS INFO line from %s:%s: %r" % (host, port, info[:80]))
        elif protocol == "postgres":
            raw.sendall(POSTGRES_SSL_REQUEST)
            answer = raw.recv(1)
            if answer != b"S":
                done(2, "postgres %s:%s refused TLS: %r" % (host, port, answer))
    except Exception as e:
        done(2, "setup failed: %r" % e)
    try:
        ctx.wrap_socket(raw, server_hostname=name).close()
    except ssl.SSLCertVerificationError as e:
        if e.verify_code == X509_V_ERR_HOSTNAME_MISMATCH:
            done(4, "refused: %s" % e.verify_message)
        done(2, "certificate verification failed for another reason: %r" % e)
    except Exception as e:
        done(2, "handshake failed: %r" % e)
    done(0, "accepted for %s" % name)


main()
