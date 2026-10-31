"""proxy-forward.py — relay TCP from the Docker bridge to the session's proxy.

install-dagger.sh runs this so the Dagger engine, and the containers it runs,
can use the agent proxy. That proxy listens on the host's loopback, which
nothing in a container can reach; this listens on the bridge gateway, which
they can, and copies bytes both ways. It never reads or rewrites them: TLS is
still between the client and the proxy.

Usage: proxy-forward.py <listen-host> <listen-port> <target-host> <target-port> <pidfile>
"""

import os
import socket
import sys
import threading


def pipe(src, dst):
    try:
        while data := src.recv(65536):
            dst.sendall(data)
    except OSError:
        pass
    finally:
        for s in (src, dst):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass


def serve(client, target):
    try:
        upstream = socket.create_connection(target)
    except OSError:
        client.close()
        return
    threading.Thread(target=pipe, args=(client, upstream), daemon=True).start()
    pipe(upstream, client)


def main():
    listen_host, listen_port, target_host, target_port, pidfile = sys.argv[1:6]
    target = (target_host, int(target_port))
    server = socket.socket()
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind((listen_host, int(listen_port)))
    server.listen(512)
    # Written once listening, so a pid in the file is a forwarder that answers.
    with open(pidfile, "w") as f:
        f.write(f"{os.getpid()}\n")
    while True:
        client, _ = server.accept()
        threading.Thread(target=serve, args=(client, target), daemon=True).start()


if __name__ == "__main__":
    main()
