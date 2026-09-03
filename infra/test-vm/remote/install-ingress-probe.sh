#!/usr/bin/env bash
set -euo pipefail

PROBE_DIR=${PROBE_DIR:-/var/lib/ebpf-wg-mesh/ingress-probe}
PROBE_LISTEN=${PROBE_LISTEN:-127.0.0.1:2019}

mkdir -p /opt/ebpf-wg-mesh "${PROBE_DIR}"

cat >/opt/ebpf-wg-mesh/ingress-probe.py <<'PY'
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

probe_dir = os.environ["PROBE_DIR"]
listen_host, listen_port = os.environ["PROBE_LISTEN"].rsplit(":", 1)


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        self.record_request()

    def do_PATCH(self):
        self.record_request()

    def record_request(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        temporary = os.path.join(probe_dir, "latest.json.tmp")
        latest = os.path.join(probe_dir, "latest.json")
        with open(temporary, "wb") as output:
            output.write(body)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, latest)
        with open(os.path.join(probe_dir, "requests.log"), "a", encoding="utf-8") as log:
            log.write(f"{self.command} {self.path}\n")
            log.flush()
            os.fsync(log.fileno())
        self.send_response(200)
        self.end_headers()

    def log_message(self, _format, *_args):
        return


ThreadingHTTPServer((listen_host, int(listen_port)), Handler).serve_forever()
PY

cat >/etc/systemd/system/ebpf-wg-mesh-ingress-probe.service <<EOF
[Unit]
Description=ebpf-wg-mesh ingress admin probe
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=PROBE_DIR=${PROBE_DIR}
Environment=PROBE_LISTEN=${PROBE_LISTEN}
ExecStart=/usr/bin/python3 /opt/ebpf-wg-mesh/ingress-probe.py
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now ebpf-wg-mesh-ingress-probe.service
