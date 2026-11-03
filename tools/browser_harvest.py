#!/usr/bin/env python3
"""browser_harvest.py - tarik cookie langsung dari Firefox lewat Marionette.

Kenapa cara ini: menyalin cookie dari file profil bisa memberi salinan yang
sudah basi (browser menyimpan nilai terbaru di memori), dan rotasi ganda antara
browser dan server membuat keduanya saling membatalkan. Di sini browser
dijalankan sendiri dengan profil SALINAN (profil asli tidak dikunci), lalu cookie
diambil dari sesi hidup lewat protokol WebDriver/Marionette.

Alur:
  1. salin profil ke temp (aman: profil asli tidak dibuka)
  2. jalankan firefox -marionette -headless dengan salinan itu
  3. WebDriver:Navigate ke gemini.google.com/app
  4. WebDriver:GetCookies  -> seluruh cookie sesi
  5. uji login (tanpa mengikuti redirect)
  6. opsional: --import, tulis ke novagemini (per akun, sesuai auth_user)

Pemakaian:
  python tools/browser_harvest.py --profile "s1d1gj83.default-release"
  python tools/browser_harvest.py --profile "s1d1gj83.default-release" --import --api-key sk-...
"""

import argparse
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

FIREFOX = r"C:\Program Files\Mozilla Firefox\firefox.exe"
PROFILE_DIR = os.path.join(os.environ["APPDATA"], "Mozilla", "Firefox", "Profiles")
GECKODRIVER = r"C:\Users\Design\AppData\Local\Microsoft\WinGet\Packages\Mozilla.GeckoDriver_Microsoft.Winget.Source_8wekyb3d8bbwe\geckodriver.exe"
WD_PORT = 4444
MARIONETTE_HOST = "127.0.0.1"
MARIONETTE_PORT = 2828
UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:147.0) Gecko/20100101 Firefox/147.0")
REQUIRED = ("SID", "SAPISID", "__Secure-1PSID")


class Marionette:
    """Klien Marionette minimal: bingkai <panjang>:<json>."""

    def __init__(self, host, port, timeout=60):
        self.sock = socket.create_connection((host, port), timeout=timeout)
        self.sock.settimeout(timeout)
        self.buf = b""
        self.next_id = 1
        # Marionette mengirim paket sapaan lebih dulu (applicationType +
        # marionetteProtocol). Kalau tidak dibuang, balasan pertama yang dibaca
        # adalah sapaan itu dan id-nya tidak pernah cocok.
        self.protocol = 3
        try:
            self.sock.settimeout(5)
            hello = self._recv_message()
            self.protocol = int(hello.get("marionetteProtocol", 3))
        except Exception:
            pass
        finally:
            self.sock.settimeout(timeout)

    def _recv_message(self):
        while b":" not in self.buf:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise ConnectionError("marionette menutup koneksi")
            self.buf += chunk
        head, _, rest = self.buf.partition(b":")
        need = int(head)
        self.buf = rest
        while len(self.buf) < need:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise ConnectionError("marionette menutup koneksi")
            self.buf += chunk
        payload, self.buf = self.buf[:need], self.buf[need:]
        return json.loads(payload.decode("utf-8"))

    def call(self, name, params=None):
        mid = self.next_id
        self.next_id += 1
        msg = json.dumps({"id": mid, "name": name, "parameters": params or {}}).encode("utf-8")
        self.sock.sendall(str(len(msg)).encode() + b":" + msg)
        while True:
            reply = self._recv_message()
            if reply.get("id") == mid:
                if "error" in reply:
                    raise RuntimeError(f"{name}: {reply['error'].get('message')}")
                return reply.get("result")


def quit_firefox():
    """Minta Firefox berhenti dengan sopan.

    Memaksa mati (taskkill /F) meninggalkan parent.lock di profil, dan Firefox
    berikutnya menolak membuka profil yang dianggap masih dipakai - itu yang
    membuat Firefox tidak bisa dibuka. taskkill tanpa /F mengirim WM_CLOSE.
    """
    for extra in ([], ["/F"]):
        try:
            subprocess.run(["taskkill", "/IM", "firefox.exe", "/T"] + extra,
                           capture_output=True, timeout=60)
        except Exception:
            pass
        time.sleep(2)
        if not _firefox_alive():
            return
    time.sleep(2)


def _firefox_alive():
    try:
        out = subprocess.run(["tasklist", "/FI", "IMAGENAME eq firefox.exe"],
                             capture_output=True, text=True, timeout=15).stdout
        return "firefox.exe" in out
    except Exception:
        return False


def kill_stale_drivers():
    """Bersihkan driver yang tertinggal.

    Geckodriver memakai port tetap (2828). Kalau satu instance sebelumnya masih
    hidup, instance berikutnya gagal mengikat port dan semua panggilan WebDriver
    berakhir HTTPError - persis yang terjadi saat penjaga dijalankan berulang.
    """
    for name in ("geckodriver.exe", "chromedriver.exe"):
        try:
            subprocess.run(["taskkill", "/IM", name, "/F"], capture_output=True, timeout=30)
        except Exception:
            pass
    time.sleep(1)


def snapshot_profile(name):
    src = os.path.join(PROFILE_DIR, name)
    if not os.path.isdir(src):
        raise SystemExit(f"profil tidak ada: {src}")
    dst = tempfile.mkdtemp(prefix="nmprofile-")
    for item in os.listdir(src):
        if item in ("Cache", "cache2", "startupCache", "lock", ".parentlock", "parent.lock"):
            continue
        s, d = os.path.join(src, item), os.path.join(dst, item)
        try:
            if os.path.isdir(s):
                shutil.copytree(s, d, symlinks=False, ignore_dangling_symlinks=True)
            else:
                shutil.copy2(s, d)
        except Exception:
            pass
    return dst


def launch(profile_dir):
    return subprocess.Popen(
        [FIREFOX, "-marionette", "-headless", "-no-remote", "-profile", profile_dir,
         "-new-instance", "about:blank"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )


def wait_marionette(seconds=45):
    deadline = time.time() + seconds
    last = ""
    while time.time() < deadline:
        try:
            return Marionette(MARIONETTE_HOST, MARIONETTE_PORT)
        except OSError as e:
            last = str(e)
            time.sleep(1)
    raise SystemExit(f"marionette tidak siap: {last}")


def build_header(cookies):
    """Cookie header dari domain Google, .google.com diutamakan."""
    best = {}
    for c in cookies:
        name, value, domain = c.get("name", ""), c.get("value", ""), c.get("domain", "")
        if not name:
            continue
        if not any(d in domain for d in ("google.com", "google.co.id")):
            continue
        rank = 2 if domain.startswith(".google.com") else (1 if "google.com" in domain else 1)
        prev = best.get(name)
        if prev is None or rank > prev[0]:
            best[name] = (rank, value)
    return "; ".join(f"{k}={v[1]}" for k, v in best.items()), {k: v[1] for k, v in best.items()}


def probe_login(header):
    req = urllib.request.Request(
        "https://gemini.google.com/app",
        headers={"Cookie": header, "User-Agent": UA,
                 "Accept": "text/html,application/xhtml+xml"})

    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *a, **k):
            return None

    op = urllib.request.build_opener(NoRedirect)
    try:
        with op.open(req, timeout=40) as r:
            body = r.read(60000)
            text = body.decode("utf-8", "replace")
            if b"SNlM0e" in body:
                return True, f"HTTP {r.status}"
            # Cadangan: halaman aplikasi Gemini memuat email akun yang login.
            # Tanpa cadangan ini, sesi yang sehat bisa dilaporkan mati hanya
            # karena halaman menyajikan bentuk lain (pernah terjadi pada akun
            # yang justru melayani request dengan baik).
            if re.search(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}", text):
                return True, f"HTTP {r.status} (email akun terlihat di halaman)"
            return False, f"HTTP {r.status}"
    except urllib.error.HTTPError as e:
        return False, f"HTTP {e.code} -> {e.headers.get('Location','')[:60]}"
    except Exception as e:  # noqa: BLE001
        return False, f"{type(e).__name__}: {e}"


def _wd(method, path, body=None, timeout=60):
    """Panggilan WebDriver HTTP ke geckodriver."""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(f"http://127.0.0.1:{WD_PORT}{path}", data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        raw = r.read()
        return json.loads(raw) if raw else {}


def harvest_gecko(profile_name):
    """Jalur utama: geckodriver + Firefox dengan profil SALINAN."""
    if not os.path.exists(GECKODRIVER):
        raise SystemExit(f"geckodriver tidak ada di {GECKODRIVER}")
    prof = snapshot_profile(profile_name)
    kill_stale_drivers()
    proc = subprocess.Popen([GECKODRIVER, f"--port={WD_PORT}"],
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
            "browserName": "firefox",
            "moz:firefoxOptions": {
                "binary": FIREFOX,
                "args": ["-headless", "-no-remote", "-profile", prof],
            },
        }}}
        session = _wd("POST", "/session", caps, timeout=120)["value"]["sessionId"]
        _wd("POST", f"/session/{session}/url", {"url": "https://gemini.google.com/app"}, timeout=120)
        time.sleep(3)
        cookies = _wd("GET", f"/session/{session}/cookie", timeout=60)["value"]
        header, table = build_header(cookies)
        return header, table, prof
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


def harvest(profile_name, keep_open=False):
    prof = snapshot_profile(profile_name)
    proc = launch(prof)
    try:
        m = wait_marionette()
        try:
            m.call("WebDriver:NewSession", {"capabilities": {}, "desiredCapabilities": {}})
        except RuntimeError as e:
            # Firefox kadang sudah punya sesi bawaan; itu bukan kegagalan.
            if "already" not in str(e).lower():
                raise
        m.call("WebDriver:Navigate", {"url": "https://gemini.google.com/app"})
        time.sleep(3)
        cookies = m.call("WebDriver:GetCookies", {}) or []
        header, table = build_header(cookies)
        return header, table, prof
    finally:
        if not keep_open:
            try:
                if proc.poll() is None:
                    proc.terminate()
            except Exception:
                pass
            shutil.rmtree(prof, ignore_errors=True)


def import_to_server(base, api_key, header, profile_name, auth_user=None, accounts=None):
    """Tulis cookie ke semua akun yang cocok (atau yang auth_user-nya disebut)."""
    url = base.rstrip("/") + "/admin/api/cookies"
    data = json.load(urllib.request.urlopen(url, timeout=20))
    items = data.get("items", [])
    if accounts:
        targets = [a for a in items if a["id"] in accounts]
    elif auth_user is not None:
        targets = [a for a in items if a.get("auth_user") == auth_user]
    else:
        # Tanpa target, menulis ke semua akun berarti menimpa sesi akun lain
        # dengan cookie milik satu akun. Itu pernah hampir merusak novaestellar.
        print("    dibatalkan: sebutkan target (--account ID atau --auth-user N)")
        return []
    written = []
    for a in targets:
        # rotate_disabled=1: akun ini "browser-owned". Server tidak boleh
        # memutar tokennya, karena browser memegang sesi yang sama dan dua
        # pemutar akan saling membatalkan. Persis itu yang menghabiskan 10 akun
        # sebelumnya: rotasi server mematikan sesi browser, lalu resync terus
        # menyalin cookie yang sudah mati.
        body = json.dumps({"cookie": header, "impersonate": "firefox_147", "auth_user": 0,  # sesi per profil = indeks 0
                           "rotate_disabled": True}).encode()
        req = urllib.request.Request(f"{url}/{a['id']}", data=body, method="PATCH",
                                     headers={"Content-Type": "application/json"})
        try:
            urllib.request.urlopen(req, timeout=20)
            written.append(a["id"])
        except urllib.error.HTTPError as e:
            print(f"    #{a['id']} gagal: {e.code} {e.read()[:80].decode('utf-8','replace')}")
    # Catat pemetaan profil -> akun supaya session_keeper tahu siapa pemilik
    # sesi tiap profil tanpa perlu ditanyakan ulang setiap kali.
    if written and profile_name:
        try:
            mp = os.path.join(os.path.dirname(os.path.abspath(__file__)), "session-map.json")
            m = {}
            if os.path.exists(mp):
                m = json.load(open(mp, encoding="utf-8"))
            m[profile_name] = sorted(set(m.get(profile_name, [])) | set(written))
            json.dump(m, open(mp, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
        except Exception:
            pass
    return written



def fetch_api_key(base):
    """Ambil API key dari panel (loopback, tanpa auth) supaya tidak perlu
    ditulis di file mana pun. Panel hanya mendengarkan 127.0.0.1."""
    try:
        import urllib.request as _u
        with _u.urlopen(base.rstrip("/") + "/admin/api/apikey", timeout=10) as r:
            import json as _j
            return _j.loads(r.read()).get("key", "") or ""
    except Exception:
        return ""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--profile", required=True, help="nama folder profil Firefox")
    ap.add_argument("--base", default="http://127.0.0.1:8083")
    ap.add_argument("--api-key", default="")
    ap.add_argument("--import", dest="do_import", action="store_true",
                    help="tulis hasilnya ke novagemini")
    ap.add_argument("--auth-user", type=int, default=None,
                    help="hanya akun dengan auth_user ini")
    ap.add_argument("--account", default="",
                    help="id akun tujuan, dipisah koma (wajib untuk --import)")
    args = ap.parse_args()
    if not getattr(args, "api_key", ""):
        args.api_key = fetch_api_key(args.base)

    print(f"  profil: {args.profile}")
    try:
        header, table, prof = harvest_gecko(args.profile)
    except Exception as e:  # noqa: BLE001
        print(f"  geckodriver gagal ({type(e).__name__}: {str(e)[:80]}), coba marionette langsung")
        header, table, prof = harvest(args.profile)
    names = sorted(table)
    print(f"  cookie google: {len(names)} ({', '.join(names[:8])}…)")
    missing = [n for n in REQUIRED if n not in table]
    if missing:
        print(f"  catatan: cookie wajib tidak ada: {missing}")

    ok, detail = probe_login(header)
    print(f"  uji login: {'HIDUP (SNlM0e ada)' if ok else 'TIDAK LOGIN'} — {detail}")

    if not ok:
        print("  tidak diimpor: sesi ini tidak lolos uji, mengimpor hanya akan menambah baris mati")
        return 2

    accounts = {int(x) for x in args.account.split(",") if x.strip().isdigit()}
    if args.do_import:
        written = import_to_server(args.base, args.api_key, header, args.profile, args.auth_user, accounts)
        print(f"  diimpor ke {len(written)} akun: {written}")
    else:
        print("  (mode uji: tidak menulis apa pun; tambahkan --import untuk menyimpan)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
