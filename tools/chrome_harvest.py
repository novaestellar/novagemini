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
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            return json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace")
        raise RuntimeError(f"HTTP {e.code} dari chromedriver: {body[:300]}") from None


def snapshot(profile_name):
    src = os.path.join(USER_DATA, profile_name)
    if not os.path.isdir(src):
        raise SystemExit(f"profil tidak ada: {src}")
    dst = tempfile.mkdtemp(prefix="nmchrome-")
    # Catatan: menyalin "Local State" ke user-data-dir tiruan membuat Chrome
    # menolak start (HTTP 500) karena kunci app-bound-nya terikat path asli.
    # Untuk cookie app-bound, pakai mode --live (profil asli, Chrome harus tutup).
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


def chrome_running():
    try:
        out = subprocess.run(["tasklist", "/FI", "IMAGENAME eq chrome.exe"],
                             capture_output=True, text=True, timeout=15).stdout
        return "chrome.exe" in out
    except Exception:
        return False


def check(profile_name, url="https://gemini.google.com/app", live=False):
    if live:
        # Profil asli: cookie app-bound hanya bisa didekripsi di path ini.
        # Syaratnya Chrome tidak sedang berjalan.
        if chrome_running():
            raise SystemExit("Chrome masih jalan — tutup dulu untuk mode --live")
        prof = os.path.join(USER_DATA, profile_name)
    else:
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
                         "--disable-sync",
                         (f"--user-data-dir={USER_DATA}" if live else f"--user-data-dir={prof}"),
                         # mode live memakai root User Data, jadi profil harus disebut
                         *([f"--profile-directory={profile_name}"] if live else [])],
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


def import_to_server(base, header, auth_user=None, accounts=None):
    """Tulis cookie Chrome ke akun yang cocok, sebagai browser-owned.

    Chrome 的新会话多数是 app-bound（device-bound）：只有创建它的浏览器能
    续期。所以这里一定带 rotate_disabled=1 —— 服务端一旦也去轮换，两边会
    互相作废，这正是十个 Firefox 账号昨天集体失效的原因。
    """
    url = base.rstrip("/") + "/admin/api/cookies"
    data = json.load(urllib.request.urlopen(url, timeout=20))
    items = data.get("items", [])
    if accounts:
        targets = [a for a in items if a["id"] in accounts]
    elif auth_user is not None:
        targets = [a for a in items if a.get("auth_user") == auth_user]
    else:
        # Menulis tanpa target = menimpa sesi akun lain dengan cookie satu
        # akun. Itu hampir merusak novaestellar; sekarang ditolak.
        print("    dibatalkan: sebutkan target (--account ID atau --auth-user N)")
        return []
    written = []
    for a in targets:
        body = json.dumps({"cookie": header, "impersonate": "chrome_146", "auth_user": 0,  # sesi per profil = indeks 0
                           "rotate_disabled": True}).encode()
        req = urllib.request.Request(f"{url}/{a['id']}", data=body, method="PATCH",
                                     headers={"Content-Type": "application/json"})
        try:
            urllib.request.urlopen(req, timeout=20)
            written.append(a["id"])
        except urllib.error.HTTPError as e:
            print(f"    #{a['id']} gagal: {e.code}")
    return written


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--profiles", nargs="*", default=None)
    ap.add_argument("--url", default="https://gemini.google.com/app")
    ap.add_argument("--base", default="http://127.0.0.1:8083")
    ap.add_argument("--import", dest="do_import", action="store_true",
                    help="tulis cookie profil yang hidup ke novagemini")
    ap.add_argument("--auth-user", type=int, default=None)
    ap.add_argument("--account", default="",
                    help="id akun tujuan, dipisah koma (wajib untuk --import)")
    ap.add_argument("--wait", type=int, default=0,
                    help="tunggu sampai Chrome tertutup (detik), lalu harvest otomatis")
    ap.add_argument("--live", action="store_true",
                    help="pakai folder profil asli (Chrome harus tertutup); perlu untuk cookie app-bound")
    args = ap.parse_args()
    accounts = {int(x) for x in args.account.split(",") if x.strip().isdigit()}

    if args.wait:
        # Chrome harus benar-benar tertutup; menutupnya dari luar sering langsung
        # dihidupkan lagi oleh background mode. Jadi kita tunggu, bukan memaksa.
        deadline = time.time() + args.wait
        while time.time() < deadline:
            if not chrome_running():
                print("  Chrome tertutup — lanjut harvest")
                break
            print(f"  menunggu Chrome ditutup... ({int(deadline - time.time())}s)", flush=True)
            time.sleep(10)
        else:
            raise SystemExit("Chrome tidak tertutup sampai batas waktu")

    profiles = args.profiles
    if not profiles:
        profiles = [d for d in sorted(os.listdir(USER_DATA))
                    if d == "Default" or d.startswith("Profile ")]
    print(f"  memeriksa {len(profiles)} profil Chrome\n")
    print("  profil            cookie  hasil")
    live = []
    for p in profiles:
        try:
            table, header = check(p, args.url, live=args.live)
            ok, detail = probe_login(header)
            print(f"  {p:17s} {len(table):>6}  {'HIDUP (SNlM0e ada)' if ok else 'tidak login'} — {detail}")
            if ok:
                live.append((p, header))
        except Exception as e:  # noqa: BLE001
            print(f"  {p:17s} {'-':>6}  gagal: {type(e).__name__}: {str(e)[:60]}")
    print()
    if live:
        print(f"  profil dengan sesi hidup: {[p for p, _ in live]}")
        if args.do_import:
            for prof_name, header in live:
                written = import_to_server(args.base, header, args.auth_user, accounts)
                print(f"  {prof_name}: diimpor ke {len(written)} akun {written}")
        os.makedirs(os.path.join(tempfile.gettempdir(), "opencode"), exist_ok=True)
        out = os.path.join(tempfile.gettempdir(), "opencode", "chrome-live-headers.json")
        json.dump({p: h for p, h in live}, open(out, "w", encoding="utf-8"))
        print(f"  header disimpan sementara di: {out}")
    else:
        print("  tidak ada profil Chrome dengan sesi Google yang hidup")
    return 0 if live else 2


if __name__ == "__main__":
    sys.exit(main())
