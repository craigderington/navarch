# lego's httpreq provider POSTs {"fqdn","value"} to /present and /cleanup.
# pebble-challtestsrv wants {"host","value"} on /set-txt and /clear-txt.
import json, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer
CT = "http://challtestsrv:8055"
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        target = "/set-txt" if self.path.rstrip("/") == "/present" else "/clear-txt"
        payload = {"host": body["fqdn"], "value": body.get("value", "")}
        req = urllib.request.Request(CT + target, json.dumps(payload).encode(),
                                     {"Content-Type": "application/json"})
        try:
            urllib.request.urlopen(req).read()
            self.send_response(200)
        except Exception as e:
            print("adapter error", e, flush=True)
            self.send_response(500)
        self.end_headers()
        print(self.path, payload["host"], flush=True)
    def log_message(self, *a): pass
HTTPServer(("0.0.0.0", 8099), H).serve_forever()
