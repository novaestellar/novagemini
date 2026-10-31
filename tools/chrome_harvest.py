#!/usr/bin/env python3
"""chrome_harvest.py - cek apakah profil Chrome punya sesi Google yang hidup.

Kenapa lewat WebDriver, bukan baca SQLite: cookie Chrome versi baru dienkripsi
dengan kunci yang terikat pada profil dan proses browser (app-bound). Membaca
file-nya dari luar menghasilkan sampah. Kalau browsernya sendiri yang membuka
profil, dekripsi terjadi di dalam browser dan nilainya bisa diambil lewat
WebDriver:GetCookies.

Profil dibuka sebagai SALINAN, jadi Chrome yang sedang Anda pakai tidak diganggu.
"""

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

CHROMEDRIVER = (r"C:\Users\Design\AppData\Local\Microsoft\WinGet\Packages"
                r"\Chromium.ChromeDriver_Microsoft.Winget.Source_8wekyb3d8bbwe"
                r"\chromedriver-win64\chromedriver.exe")
CHROME = r"C:\Program Files\Google\Chrome\Application\chrome.exe"
USER_DATA = os.path.join(os.environ["LOCALAPPDATA"], "Google", "Chrome", "User Data")
WD_PORT = 9515
UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36")


def _wd(method, path, body=None, timeout=90):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(f"http://127.0.0.1:{WD_PORT}{path}", data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        raw = r.read()
        return json.loads(raw) if raw else {}


def snapshot(profile_name):
    src = os.path.join(USER_DATA, profile_name)
    if not os.path.isdir(src):
        raise SystemExit(f"profil tidak ada: {src}")
    dst = tempfile.mkdtemp(prefix="nmchrome-")
    skip = {"Cache", "Code Cache", "GPUCache", "GrShaderCache", "ShaderCache",
            "DawnCache", "DawnGraphiteCache", "DawnWebGPUCache", "component_crx_cache",
            "extensions_crx_cache", "Crashpad", "BrowserMetrics", "Safe Browsing"}
    for item in os.listdir(src):
        if item in skip:
            continue
        s, d = os.path.join(src, item), os.path.join(dst, item)
        try:
            if os.path.isdir(s):
                shutil.copytree(s, d, symlinks=False, ignore_dangling_symlinks=True,
                                ignore=shutil.ignore_patterns(*skip))
            else:
                shutil.copy2(s, d)
        except Exception:
            pass
    return dst


def probe_login(header):
    req = urllib.request.Request("https://gemini.google.com/app",
                                 headers={"Cookie": header, "User-Agent": UA,
                                          "Accept": "text/html,application/xhtml+xml"})

    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *a, **k):
            return None

    try:
        with urllib.request.build_opener(NoRedirect).open(req, timeout=40) as r:
            return (b"SNlM0e" in r.read(40000)), f"HTTP {r.status}"
    except urllib.error.HTTPError as e:
        return False, f"HTTP {e.code} -> {e.headers.get('Location','')[:50]}"
    except Exception as e:  # noqa: BLE001
        return False, f"{type(e).__name__}: {str(e)[:60]}"


def build_header(cookies):
    best = {}
    for c in cookies:
        name, value, domain = c.get("name", ""), c.get("value", ""), c.get("domain", "")
        if not name or not any(d in domain for d in ("google.com", "google.co.id")):
            continue
        rank = 2 if domain.startswith(".google.com") else 1
        prev = best.get(name)
        if prev is None or rank > prev[0]:
            best[name] = (rank, value)
    return "; ".join(f"{k}={v[1]}" for k, v in best.items()), {k: v[1] for k, v in best.items()}


def check(profile_name, url="https://gemini.google.com/app"):
    prof = snapshot(profile_name)
    proc = subprocess.Popen([CHROMEDRIVER, f"--port={WD_PORT}"],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    session = None
    try:
        deadline = time.time() + 30
        while time.time() < deadline:
            try:
                _wd("GET", "/status", timeout=5)
                break
            except Exception:
                time.sleep(1)
        caps = {"capabilities": {"alwaysMatch": {
            "browserName": "chrome",
            "goog:chromeOptions": {
                "binary": CHROME,
                "args": ["--headless=new", "--no-first-run", "--no-default-browser-check",
                         "--disable-sync", f"--user-data-dir={prof}"],
            },
        }}}
        session = _wd("POST", "/session", caps, timeout=120)["value"]["sessionId"]
        _wd("POST", f"/session/{session}/url", {"url": url}, timeout=120)
        time.sleep(3)
        cookies = _wd("GET", f"/session/{session}/cookie", timeout=60)["value"]
        header, table = build_header(cookies)
        return table, header
    finally:
        try:
            if session:
                _wd("DELETE", f"/session/{session}", timeout=30)
        except Exception:
            pass
        try:
            proc.terminate()
        except Exception:
            pass
        shutil.rmtree(prof, ignore_errors=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--profiles", nargs="*", default=None)
    ap.add_argument("--url", default="https://gemini.google.com/app")
    args = ap.parse_args()

    profiles = args.profiles
    if not profiles:
        profiles = [d for d in sorted(os.listdir(USER_DATA))
                    if d == "Default" or d.startswith("Profile ")]
    print(f"  memeriksa {len(profiles)} profil Chrome\n")
    print("  profil            cookie  hasil")
    live = []
    for p in profiles:
        try:
            table, header = check(p, args.url)
            ok, detail = probe_login(header)
            print(f"  {p:17s} {len(table):>6}  {'HIDUP (SNlM0e ada)' if ok else 'tidak login'} — {detail}")
            if ok:
                live.append((p, header))
        except Exception as e:  # noqa: BLE001
            print(f"  {p:17s} {'-':>6}  gagal: {type(e).__name__}: {str(e)[:60]}")
    print()
    if live:
        print(f"  profil dengan sesi hidup: {[p for p, _ in live]}")
        os.makedirs(os.path.join(tempfile.gettempdir(), "opencode"), exist_ok=True)
        out = os.path.join(tempfile.gettempdir(), "opencode", "chrome-live-headers.json")
        json.dump({p: h for p, h in live}, open(out, "w", encoding="utf-8"))
        print(f"  header disimpan sementara di: {out}")
    else:
        print("  tidak ada profil Chrome dengan sesi Google yang hidup")
    return 0 if live else 2


if __name__ == "__main__":
    sys.exit(main())
