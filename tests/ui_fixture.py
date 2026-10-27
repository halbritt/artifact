#!/usr/bin/env python3
"""Run artifactd with disposable data and publish a deterministic UI fixture."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import urllib.request

port = sys.argv[1]
base = f"http://127.0.0.1:{port}"
with tempfile.TemporaryDirectory(prefix="artifact-ui-") as temp:
    root = Path(temp)
    proc = subprocess.Popen(["bin/artifactd", "--listen", f"127.0.0.1:{port}", "--data", str(root / "store"), "--base-url", base])
    try:
        for attempt in range(100):
            if proc.poll() is not None:
                raise RuntimeError("artifactd exited during startup")
            try:
                with urllib.request.urlopen(base + "/healthz", timeout=1):
                    break
            except OSError:
                time.sleep(0.05)
        else:
            raise RuntimeError("artifactd did not start")
        files = root / "files"
        files.mkdir()
        (files / "report.html").write_text("<!doctype html><html lang='en'><meta name='viewport' content='width=device-width,initial-scale=1'><title>Sample report</title><main><h1>Sample report</h1><button onclick=\"this.textContent='Working'\">Test interaction</button></main></html>")
        (files / "measurements.csv").write_text("x,y\n1,2\n")
        receipt = json.loads(subprocess.check_output(["bin/artifact", "publish", str(files), "--title", "Routing review", "--json", "--server", base]))
        # Fixtures use a known ID so UIPass has stable routes. Only temporary bytes change.
        old = receipt["id"]
        published = root / "store" / "published"
        os.rename(published / old, published / "sample")
        manifest = published / "sample" / "_artifact" / "manifest.json"
        manifest.write_text(manifest.read_text().replace(f"/{old}/", "/sample/").replace(f'"{old}"', '"sample"'))
        proc.wait()
    finally:
        proc.terminate()
        proc.wait(timeout=20)
