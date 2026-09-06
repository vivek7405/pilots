#!/usr/bin/env python3
"""A GitHub API stand-in for gate.sh section 21.

The push path is the one deploy surface with no client on the other end: a
delivery arrives, one host acts, and everything after that is a build log and a
journal line. Without something to answer `GET /repos/.../tarball/...` there is
no way to exercise it on the rig at all, because the rig has no GitHub App and
no repository.

So hostd reads its API base from PILOT_GITHUB_API_URL and this answers the three
calls the push path makes:

    POST /app/installations/<n>/access_tokens   -> {"token": "t"}
    GET  /repos/<owner>/<repo>/tarball/<ref>    -> a gzipped tarball
    POST /repos/.../issues/<n>/comments         -> 201, ignored

The tarball is served with the single wrapping directory GitHub adds, because
StripRoot removes exactly one and a tarball without it loses the top level of
the repository.

Run it on the operator machine, where the hosts can reach it:

    python3 scripts/cluster/fake-github.py --port 9418 \\
        --tar /tmp/webjs-app.tar.gz --tar-multi /tmp/workspace-app.tar.gz
"""

import argparse
import json
import re
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TARBALL = re.compile(r"^/repos/[^/]+/([^/]+)/tarball/")


class Handler(BaseHTTPRequestHandler):
    # One tarball per repository name, filled from the command line.
    tarballs: dict = {}

    def log_message(self, fmt, *args):
        # One line per call, on stderr, so a failing section can be read back.
        sys.stderr.write("fake-github: %s\n" % (fmt % args))

    def _send(self, status, body, content_type):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path.endswith("/access_tokens"):
            # Never checked for expiry by the caller: it mints one per job.
            self._send(201, json.dumps({"token": "t"}).encode(), "application/json")
            return
        # A comment, or anything else the push path posts. Accepted and
        # discarded: what the comment SAYS is asserted in Go, not here.
        self._send(201, b"{}", "application/json")

    def do_GET(self):
        match = TARBALL.match(self.path)
        if not match:
            self._send(404, b'{"message":"no route"}', "application/json")
            return
        repo = match.group(1)
        blob = self.tarballs.get(repo)
        if blob is None:
            self._send(404, json.dumps({"message": "no tarball for %s" % repo}).encode(),
                       "application/json")
            return
        self._send(200, blob, "application/gzip")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=9418)
    ap.add_argument("--repo", action="append", default=[],
                    help="<name>=<path to a .tar.gz>, repeatable")
    args = ap.parse_args()

    for pair in args.repo:
        name, _, path = pair.partition("=")
        if not path:
            ap.error("--repo takes <name>=<path>")
        with open(path, "rb") as fh:
            Handler.tarballs[name] = fh.read()
    if not Handler.tarballs:
        ap.error("no --repo given; there is nothing to serve")

    server = ThreadingHTTPServer(("0.0.0.0", args.port), Handler)
    sys.stderr.write("fake-github: serving %s on :%d\n"
                     % (", ".join(sorted(Handler.tarballs)), args.port))
    server.serve_forever()


if __name__ == "__main__":
    main()
