#!/usr/bin/env python3
"""cookie_grab.py — ambil cookie sesi Google dari browser lokal, verifikasi, lalu
(opsional) daftarkan ke pool NovaGemini.

Kenapa alat ini ada: sesi Google yang disimpan browser bisa dipakai ulang untuk
NovaGemini, tapi mengambilnya manual rawan salah (kurang satu cookie, domain
salah, akun cuma jadi tamu). Alat ini menutup tiga hal yang paling sering
menggagalkan:

  1. Chrome mengunci database cookie saat berjalan -> selalu menyalin dulu.
  2. Nilai cookie terenkripsi AES-256-GCM dengan kunci dari `Local State`
     (dibungkus DPAPI) -> didekripsi di sini.
  3. Cookie yang "ada" belum tentu sesi hidup -> diverifikasi ke /app dengan
     cek SNlM0e, penanda yang sama yang dipakai NovaGemini.

Perintah:
    python cookie_grab.py list                 # profil + status login (tanpa nilai cookie)
    python cookie_grab.py check --profile P    # verifikasi satu profil
    python cookie_grab.py grab --profile P     # cetak header (redacted) + tulis ke file
    python cookie_grab.py import --profile P   # verifikasi lalu daftarkan ke NovaGemini
    python cookie_grab.py import --login-only  # daftarkan semua profil yang login

Nilai cookie TIDAKPERNAH dicetak penuh ke layar. Yang ditulis ke file hanya
kalau --out diberikan, dan file itu dibuat dengan izin terbatas.
"""

from __future__ import annotations

import argparse
import base64
import ctypes
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.request
from ctypes import wintypes
from pathlib import Path

# Cookie yang wajib ada agar NovaGemini bisa menghitung SAPISIDHASH dan
# mempertahankan sesi. Urutan ini juga urutan penulisan header.
PREFERRED = [
    "SID", "HSID", "SSID", "APISID", "SAPISID",
    "__Secure-1PSID", "__Secure-3PSID",
    "__Secure-1PAPISID", "__Secure-3PAPISID",
    "__Secure-1PSIDTS", "__Secure-3PSIDTS",
    "SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDCC",
    "NID", "AEC", "__Secure-BUCKET",
]
REQUIRED = ("SAPISID", "__Secure-1PSID", "SID")

GOOGLE_DOMAIN_SUFFIXES = (".google.com", "google.com", ".google.co.id", "google.co.id")

UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")

# ── Windows DPAPI ────────────────────────────────────────────────────────────

class DATA_BLOB(ctypes.Structure):
    _fields_ = [("cbData", wintypes.DWORD), ("pbData", ctypes.POINTER(ctypes.c_char))]


def _dpapi_unprotect(data: bytes) -> bytes:
    """CryptUnprotectData — membuka bungkus DPAPI milik user yang sedang login."""
    blob_in = DATA_BLOB(len(data), ctypes.cast(ctypes.create_string_buffer(data),
                                               ctypes.POINTER(ctypes.c_char)))
    blob_out = DATA_BLOB()
    if not ctypes.windll.crypt32.CryptUnprotectData(
            ctypes.byref(blob_in), None, None, None, None, 0, ctypes.byref(blob_out)):
        raise OSError("CryptUnprotectData gagal (bukan user pemilik cookie?)")
    try:
        return ctypes.string_at(blob_out.pbData, blob_out.cbData)
    finally:
        ctypes.windll.kernel32.LocalFree(blob_out.pbData)


def _master_key(local_state: Path) -> bytes:
    """Ambil kunci AES-256 dari Local State, buka bungkus DPAPI-nya."""
    state = json.loads(local_state.read_text(encoding="utf-8"))
    enc = state.get("os_crypt", {}).get("encrypted_key")
    if not enc:
        raise ValueError("Local State tidak punya os_crypt.encrypted_key")
    raw = base64.b64decode(enc)
    if raw.startswith(b"DPAPI"):
        raw = raw[5:]
    return _dpapi_unprotect(raw)


def _decrypt_value(enc: bytes, key: bytes, v20_ok: bool = False) -> str | None:
    """Dekripsi satu encrypted_value. None = tidak bisa (mis. v20 app-bound)."""
    if not enc:
        return ""
    prefix = enc[:3]
    if prefix in (b"v10", b"v11"):
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM
        nonce, payload = enc[3:15], enc[15:]
        if len(payload) < 16:
            return None
        try:
            return AESGCM(key).decrypt(nonce, payload, None).decode("utf-8")
        except Exception:
            return None
    if prefix == b"v20":
        # Chrome 127+ app-bound encryption: kunci terikat ke proses browser,
        # tidak bisa dibuka lewat DPAPI. Dilaporkan, bukan ditebak.
        return None
    # Sebagian build lama menyimpan plaintext.
    try:
        return enc.decode("utf-8")
    except Exception:
        return None


# ── Penemuan profil ──────────────────────────────────────────────────────────

CHROMIUM_VENDORS = [
    ("chrome", "Chrome", Path(os.environ.get("LOCALAPPDATA", "")) / "Google/Chrome/User Data"),
    ("chrome-beta", "Chrome Beta", Path(os.environ.get("LOCALAPPDATA", "")) / "Google/Chrome Beta/User Data"),
    ("edge", "Edge", Path(os.environ.get("LOCALAPPDATA", "")) / "Microsoft/Edge/User Data"),
    ("brave", "Brave", Path(os.environ.get("LOCALAPPDATA", "")) / "BraveSoftware/Brave-Browser/User Data"),
    ("vivaldi", "Vivaldi", Path(os.environ.get("LOCALAPPDATA", "")) / "Vivaldi/User Data"),
    ("opera", "Opera", Path(os.environ.get("APPDATA", "")) / "Opera Software/Opera Stable"),
]


def discover() -> list[dict]:
    """Daftar profil yang punya database cookie, tanpa decrypt apa pun."""
    out: list[dict] = []
    for vid, vname, root in CHROMIUM_VENDORS:
        if not root.is_dir():
            continue
        for prof in sorted(root.iterdir()):
            if not prof.is_dir():
                continue
            if prof.name not in ("Default",) and not prof.name.startswith("Profile "):
                continue
            db = prof / "Network" / "Cookies"
            if not db.exists():
                db = prof / "Cookies"
            if not db.exists():
                continue
            ls = root / "Local State"
            out.append({
                "id": f"{vid}:{prof.name}",
                "browser": vname,
                "profile": prof.name,
                "kind": "chromium",
                "db": db,
                "local_state": ls,
            })
    ff_root = Path(os.environ.get("APPDATA", "")) / "Mozilla/Firefox/Profiles"
    if ff_root.is_dir():
        for prof in sorted(ff_root.iterdir()):
            db = prof / "cookies.sqlite"
            if db.exists():
                out.append({
                    "id": f"firefox:{prof.name}",
                    "browser": "Firefox",
                    "profile": prof.name,
                    "kind": "firefox",
                    "db": db,
                    "local_state": None,
                })
    return out


def _read_locked(path: Path) -> bytes:
    """Baca file yang sedang dipegang proses lain (Chrome mengunci DB-nya).

    shutil.copy2 gagal dengan WinError 32 karena Chrome membuka file dengan
    share mode terbatas. CreateFileW dengan FILE_SHARE_* lebih permisif: kita
    hanya minta baca, dan Windows mengizinkan selama pemiliknya juga membuka
    share read — yang memang dilakukan Chrome.
    """
    GENERIC_READ = 0x80000000
    FILE_SHARE_ALL = 0x1 | 0x2 | 0x4  # READ | WRITE | DELETE
    OPEN_EXISTING = 3
    handle = ctypes.windll.kernel32.CreateFileW(
        str(path), GENERIC_READ, FILE_SHARE_ALL, None, OPEN_EXISTING, 0, None)
    if handle == -1 or handle == 0xFFFFFFFFFFFFFFFF:
        raise OSError(f"tidak bisa membuka {path} (error {ctypes.GetLastError()})")
    try:
        chunks = []
        buf = ctypes.create_string_buffer(1 << 20)
        read = wintypes.DWORD(0)
        while True:
            ok = ctypes.windll.kernel32.ReadFile(
                handle, buf, len(buf), ctypes.byref(read), None)
            if not ok or read.value == 0:
                break
            chunks.append(buf.raw[:read.value])
        return b"".join(chunks)
    finally:
        ctypes.windll.kernel32.CloseHandle(handle)


def _snapshot_db(db: Path) -> Path:
    """Snapshot konsisten lewat SQLite backup API.

    Menyalin file mentah tidak cukup untuk Firefox: semua perubahan terbaru
    (termasuk cookie login yang baru saja ditulis) masih berada di `-wal`, dan
    salinan yang dibuka read-only mengabaikan WAL — hasilnya profil yang jelas
    login terbaca sebagai tamu. Backup API membaca lewat engine SQLite, jadi
    WAL ikut ter-replay di snapshot.
    """
    src = sqlite3.connect(f"file:{db}?mode=ro", uri=True, timeout=5)
    try:
        tmp = Path(tempfile.mkdtemp(prefix="cgrab-")) / "Cookies"
        dst = sqlite3.connect(str(tmp))
        try:
            with dst:
                src.backup(dst)
        finally:
            dst.close()
        return tmp
    finally:
        src.close()


def _copy_db(db: Path) -> Path:
    """Salin DB ke temp: Chrome mengunci file saat berjalan."""
    tmpdir = Path(tempfile.mkdtemp(prefix="cgrab-"))
    dst = tmpdir / "Cookies"
    dst.write_bytes(_read_locked(db))
    for suffix in ("-wal", "-shm"):
        side = Path(str(db) + suffix)
        if side.exists():
            try:
                Path(str(dst) + suffix).write_bytes(_read_locked(side))
            except OSError:
                pass
    return dst


def read_cookies(entry: dict) -> tuple[dict[str, str], list[str]]:
    """(cookies, masalah) — cookies berupa name->value untuk domain Google."""
    problems: list[str] = []
    try:
        # Utamakan backup API (menangani WAL). Chrome 127+ menahan DB dengan
        # share mode eksklusif sehingga jalur ini gagal — baru jatuh ke salinan.
        try:
            db_copy = _snapshot_db(entry["db"])
        except Exception:
            db_copy = _copy_db(entry["db"])
    except OSError as e:
        # Chrome 127+ menahan database cookie dengan share mode eksklusif
        # (bagian dari hardening app-bound encryption). Tidak ada cara membacanya
        # dari luar tanpa VSS atau DevTools; laporkan, jangan gagal senyap.
        problems.append(
            f"database terkunci browser ({e}); tutup browser atau pakai mode --cdp")
        return {}, problems
    cookies: dict[str, str] = {}
    best: dict[str, tuple[int, str]] = {}

    def take(name: str, value: str, host: str) -> None:
        """Simpan nilai terbaik per nama berdasarkan peringkat domain."""
        if not name or not value:
            return
        rank = COOKIE_NAME_RANK.get(host.lower(), 5)
        cur = best.get(name)
        if cur is None or rank < cur[0]:
            best[name] = (rank, value)
    try:
        con = sqlite3.connect(f"file:{db_copy}?mode=ro", uri=True)
        try:
            if entry["kind"] == "firefox":
                rows = con.execute(
                    "SELECT name, value, host FROM moz_cookies "
                    "WHERE host LIKE '%google.com' OR host LIKE '%google.co.id'"
                ).fetchall()
                for name, value, host in rows:
                    take(name, value, host)
            else:
                key = _master_key(entry["local_state"]) if entry["local_state"] else b""
                rows = con.execute(
                    "SELECT name, value, encrypted_value, host_key, expires_utc FROM cookies "
                    "WHERE host_key LIKE '%google.com' OR host_key LIKE '%google.co.id' "
                    "ORDER BY expires_utc ASC"
                ).fetchall()
                v20 = 0
                for name, value, enc, host, _exp in rows:
                    if value:
                        take(name, value, host)
                        continue
                    dec = _decrypt_value(enc or b"", key)
                    if dec is None:
                        if (enc or b"").startswith(b"v20"):
                            v20 += 1
                        continue
                    if dec:
                        take(name, dec, host)
                if v20:
                    problems.append(
                        f"{v20} cookie memakai v20 (app-bound) — tidak bisa dibuka di luar "
                        f"browser; jalankan profile ini dengan Chrome yang sama atau ekspor manual")
        finally:
            con.close()
    finally:
        shutil.rmtree(db_copy.parent, ignore_errors=True)
    for name, (_rank, value) in best.items():
        cookies[name] = value
    return cookies, problems


# ── Verifikasi sesi ──────────────────────────────────────────────────────────

def build_header(cookies: dict[str, str]) -> str:
    parts = []
    for name in PREFERRED:
        if name in cookies:
            parts.append(f"{name}={cookies[name]}")
    # Sertakan cookie Google lain yang belum tercakup daftar (mis. __Secure-ENID).
    for name, value in cookies.items():
        if name not in PREFERRED and value:
            parts.append(f"{name}={value}")
    return "; ".join(parts)


def verify(header: str, timeout: int = 45) -> tuple[bool, str]:
    """(login?, detail). Login = /app membawa SNlM0e."""
    req = urllib.request.Request(
        "https://gemini.google.com/app",
        headers={
            "Cookie": header,
            "User-Agent": UA,
            "Accept": "text/html,application/xhtml+xml",
            "Accept-Language": "en-US,en;q=0.9",
            "Accept-Encoding": "identity",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            body = r.read()
            if b"SNlM0e" in body:
                return True, f"HTTP {r.status}, sesi hidup (SNlM0e ada)"
            guest = b"accounts.google.com" in body
            return False, (f"HTTP {r.status}, tidak ada SNlM0e"
                           f"{' — Google menganggap tamu' if guest else ''}")
    except Exception as e:
        return False, f"{type(e).__name__}: {str(e)[:120]}"


def redact(value: str) -> str:
    if len(value) <= 10:
        return "*" * len(value)
    return f"{value[:4]}…{value[-3:]} ({len(value)} chars)"


# ── NovaGemini ───────────────────────────────────────────────────────────────

def novagemini_add(base: str, api_key: str, label: str, cookie: str, note: str,
                   auth_user: int = 0) -> dict:
    body = json.dumps({"label": label, "cookie": cookie, "note": note,
                       "auth_user": auth_user}).encode()
    req = urllib.request.Request(
        f"{base.rstrip('/')}/admin/api/cookies", data=body, method="POST",
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {api_key}"},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read().decode("utf-8", "replace"))


def existing_tails(base: str, api_key: str) -> set[str]:
    """Sidik akun yang sudah terdaftar: `<tail SAPISID>@<auth_user>`.

    Dipakai untuk mencegah akun yang sama masuk dua kali saat alat ini dijalankan
    berulang. Panel sudah menyediakan `sapisid_tail`, jadi pembandingnya stabil
    tanpa perlu mengirim cookie utuh bolak-balik.

    Nomor authuser ikut masuk kunci: satu profil browser bisa memegang beberapa
    akun dengan cookie yang PERSIS sama, dan tanpa nomor itu akun kedua, ketiga,
    dan seterusnya dianggap duplikat dari yang pertama.
    """
    req = urllib.request.Request(f"{base.rstrip('/')}/admin/api/cookies",
                                 headers={"Authorization": f"Bearer {api_key}"})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            data = json.loads(r.read().decode("utf-8", "replace"))
    except Exception:
        return set()
    out: set[str] = set()
    for i in data.get("items", []):
        tail = str(i.get("sapisid_tail") or "")
        if not tail:
            continue
        out.add(f"{tail}@{int(i.get('auth_user') or 0)}")
    return out


def sapisid_tail(header: str) -> str:
    for part in header.split(";"):
        name, _, value = part.strip().partition("=")
        if name == "SAPISID" and len(value) >= 4:
            return value[-4:]
    return ""



# ── Jalur CDP: minta Chrome membuka cookie-nya sendiri ───────────────────────
#
# Sejak Chrome 127 cookie Google disimpan dengan enkripsi "app-bound" (prefix
# v20). Kuncinya dipegang proses browser, bukan DPAPI, jadi membaca database
# dari luar hanya menghasilkan baris yang tidak bisa didekripsi — dan Chrome
# membuangnya diam-diam saat kita pindai lewat salinan profil (terukur: 6
# cookie non-auth tersisa, SID/SAPISID hilang).
#
# Satu-satunya pihak yang bisa membukanya adalah Chrome itu sendiri. Karena itu
# jalur ini menyalakan Chrome dengan port debug, meminta `Network.getAllCookies`
# lewat CDP, lalu menutupnya lagi. Satu profil per putaran karena satu proses
# Chrome hanya melayani satu profil saat start.

CHROME_CANDIDATES = [
    r"C:\Program Files\Google\Chrome\Application\chrome.exe",
    r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe",
    os.path.expandvars(r"%LOCALAPPDATA%\Google\Chrome\Application\chrome.exe"),
]

COOKIE_NAME_RANK = {
    "gemini.google.com": -1,
    # .google.com menang atas domain negara: sesi Google yang dipakai Gemini
    # selalu di .google.com. Profil yang pernah login di dua region menyimpan
    # dua set lengkap, dan memilih .google.co.id menghasilkan halaman "Sign in"
    # walau akunnya jelas aktif — kasus nyata di profil Firefox operator.
    ".google.com": 0,
    "google.com": 0,
    "accounts.google.com": 1,
    ".google.co.id": 3,
    "google.co.id": 3,
}


def chrome_exe() -> str | None:
    for p in CHROME_CANDIDATES:
        if os.path.exists(p):
            return p
    return None


def _free_port() -> int:
    import socket
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _kill_tree(proc) -> None:
    try:
        subprocess.run(["taskkill", "/F", "/T", "/PID", str(proc.pid)],
                       capture_output=True, timeout=20)
    except Exception:
        pass
    try:
        proc.wait(timeout=8)
    except Exception:
        try:
            proc.kill()
        except Exception:
            pass


def _cdp_cookies(port: int, timeout: float = 40.0) -> list[dict]:
    """Ambil semua cookie dari Chrome yang sedang jalan lewat CDP."""
    import asyncio
    import websockets

    async def run() -> list[dict]:
        ver = json.loads(urllib.request.urlopen(
            f"http://127.0.0.1:{port}/json/version", timeout=10).read())
        async with websockets.connect(ver["webSocketDebuggerUrl"],
                                      max_size=64 * 1024 * 1024) as ws:
            mid = [0]

            async def cmd(method: str, params: dict | None = None, session: str | None = None):
                mid[0] += 1
                msg = {"id": mid[0], "method": method, "params": params or {}}
                if session:
                    msg["sessionId"] = session
                await ws.send(json.dumps(msg))
                while True:
                    r = json.loads(await asyncio.wait_for(ws.recv(), timeout=timeout))
                    if r.get("id") == mid[0]:
                        return r

            t = await cmd("Target.createTarget", {"url": "about:blank"})
            tid = t.get("result", {}).get("targetId")
            if not tid:
                return []
            a = await cmd("Target.attachToTarget", {"targetId": tid, "flatten": True})
            sid = a.get("result", {}).get("sessionId")
            await cmd("Network.enable", {}, sid)
            r = await cmd("Network.getAllCookies", {}, sid)
            return r.get("result", {}).get("cookies", [])

    return asyncio.run(run())


def _header_from_cdp(cookies: list[dict]) -> tuple[str, int]:
    """Rakit header dari hasil CDP: hanya domain Google, nama terkuat menang."""
    best: dict[str, tuple[int, str]] = {}
    for c in cookies:
        domain = (c.get("domain") or "").lower()
        if not (domain.endswith("google.com") or domain.endswith("google.co.id")):
            continue
        name, value = c.get("name") or "", c.get("value") or ""
        if not name or not value:
            continue
        rank = COOKIE_NAME_RANK.get(domain, 5)
        prev = best.get(name)
        if prev is None or rank < prev[0]:
            best[name] = (rank, value)
    ordered = []
    for name in PREFERRED:
        if name in best:
            ordered.append(f"{name}={best[name][1]}")
    for name, (_rank, value) in best.items():
        if name not in PREFERRED:
            ordered.append(f"{name}={value}")
    return "; ".join(ordered), len(best)


def _junction(link: Path, target: Path) -> None:
    subprocess.run(["cmd", "/c", "mklink", "/J", str(link), str(target)],
                   capture_output=True, timeout=30)


def _rmdir_junction(link: Path) -> None:
    subprocess.run(["cmd", "/c", "rmdir", str(link)], capture_output=True, timeout=30)


def harvest_chrome_profile(profile: str, user_data: Path, wait: float = 90.0,
                           use_junction: bool = True) -> tuple[str, int, list[str]]:
    """Nyalakan Chrome pada profil asli lewat junction, ambil cookie, tutup lagi.

    Kenapa junction: Chrome 136+ menolak membuka port debug kalau
    `--user-data-dir` menunjuk folder default (proteksi terhadap pencurian
    cookie). Menyalin profil tidak menolong — kunci app-bound hanya dibuka untuk
    profil aslinya, dan hasil ukur menunjukkan salinan menghasilkan 0 cookie.
    Junction menunjuk folder yang SAMA lewat path lain, sehingga Chrome melihat
    "bukan default" sementara datanya tetap asli: kunci, sesi, dan profil utuh.
    """
    exe = chrome_exe()
    if not exe:
        raise SystemExit("chrome.exe tidak ditemukan")
    notes: list[str] = []
    if use_junction:
        link = Path(tempfile.gettempdir()) / "opencode" / f"udd-{os.getpid()}-{int(time.time() % 100000)}"
        link.parent.mkdir(parents=True, exist_ok=True)
        if link.exists():
            _rmdir_junction(link)
        _junction(link, user_data)
        if not link.exists():
            return "", 0, ["junction gagal dibuat"]
    else:
        link = user_data

    port_file = link / "DevToolsActivePort"
    if port_file.exists():
        try:
            port_file.unlink()
        except OSError:
            pass

    proc = subprocess.Popen(
        [exe, f"--user-data-dir={link}", f"--profile-directory={profile}",
         "--remote-debugging-port=0", "--no-first-run", "--no-default-browser-check",
         "about:blank"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        deadline = time.time() + wait
        port = 0
        while time.time() < deadline:
            if port_file.exists():
                lines = port_file.read_text(errors="replace").splitlines()
                if len(lines) >= 2 and lines[0].strip().isdigit():
                    port = int(lines[0].strip())
                    break
            if proc.poll() is not None:
                notes.append("Chrome keluar saat start")
                return "", 0, notes
            time.sleep(0.4)
        if not port:
            notes.append("port debug tidak terbuka")
            return "", 0, notes
        cookies = _cdp_cookies(port)
        header, count = _header_from_cdp(cookies)
        return header, count, notes
    finally:
        _kill_tree(proc)
        if use_junction:
            # Mode profil asli: pastikan tidak ada sisa proses yang memegang folder.
            _kill_tree_all("chrome.exe")
        time.sleep(1.5)
        try:
            port_file.unlink()
        except OSError:
            pass
        if use_junction:
            _rmdir_junction(link)


def cmd_chrome(args) -> int:
    dedicated = Path(os.path.expandvars(args.user_data_dir)) if args.user_data_dir else None
    user_data = dedicated or Path(os.path.expandvars(r"%LOCALAPPDATA%\Google\Chrome\User Data"))
    if not user_data.is_dir() and not dedicated:
        raise SystemExit("folder User Data Chrome tidak ditemukan")

    # Mode folder khusus: Chrome boleh berjalan berdampingan dengan browser
    # operator (singleton terkunci per user-data-dir), jadi tidak ada yang perlu
    # ditutup. Profil khusus ini juga membuat kunci app-bound dibuat khusus
    # untuk folder itu, sehingga cookie-nya bisa dibaca lagi kapan saja.
    if dedicated:
        user_data.mkdir(parents=True, exist_ok=True)

    if args.login:
        exe = chrome_exe()
        if not exe:
            raise SystemExit("chrome.exe tidak ditemukan")
        subprocess.Popen(
            [exe, f"--user-data-dir={user_data}", "--no-first-run", "--no-default-browser-check",
             "https://gemini.google.com/app"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        print(f"  Chrome dibuka dengan profil khusus:\n    {user_data}\n"
              f"  Login ke akun Gemini di jendela itu, lalu jalankan perintah yang sama\n"
              f"  tanpa --login untuk mengambil cookienya.")
        return 0

    if args.profiles:
        wanted = [p.strip() for p in args.profiles.split(",") if p.strip()]
    else:
        wanted = sorted(p.name for p in user_data.iterdir()
                        if p.is_dir() and (p.name == "Default" or p.name.startswith("Profile "))
                        and (p / "Network" / "Cookies").exists()) or ["Default"]

    if not dedicated:
        running = "chrome.exe" in subprocess.run(
            ["tasklist", "/FI", "IMAGENAME eq chrome.exe"],
            capture_output=True, text=True).stdout.lower()
        if running:
            print("  Chrome ditutup dulu: kunci cookie hanya dibuka untuk proses yang\n"
                  "  memegangnya, dan satu folder profil hanya bisa dipakai satu proses.\n"
                  "  Tab dipulihkan saat Chrome dinyalakan kembali di akhir.")
            _kill_tree_all("chrome.exe")
            time.sleep(2.5)

    results = []
    known = existing_tails(args.base, args.api_key) if args.api_key else set()
    for prof in wanted:
        print(f"\n  [{prof}] membaca cookie…", flush=True)
        header, count, notes = harvest_chrome_profile(prof, user_data,
                                                      use_junction=not dedicated)
        for n in notes:
            print(f"    catatan: {n}")
        if not header:
            print("    tidak ada cookie")
            results.append((prof, False, "tidak ada cookie"))
            continue
        missing = [n for n in REQUIRED if f"{n}=" not in header]
        if missing:
            print(f"    cookie wajib kurang: {', '.join(missing)}")
            results.append((prof, False, "cookie tidak lengkap"))
            continue
        ok, detail = verify(header)
        print(f"    {count} cookie google — {'LOGIN' if ok else 'TIDAK LOGIN'} ({detail})")
        if ok and args.api_key:
            tail = sapisid_tail(header)
            if tail and tail in known:
                print(f"    sudah terdaftar (SAPISID …{tail}), tidak digandakan")
                results.append((prof, True, f"duplikat …{tail}"))
                continue
            try:
                res = novagemini_add(args.base, args.api_key,
                                     args.label or f"chrome-{prof.replace(' ', '').lower()}",
                                     header, f"cookie_grab via CDP dari {prof}")
                print(f"    didaftarkan ke NovaGemini sebagai #{res.get('id')}")
                results.append((prof, True, f"akun #{res.get('id')}"))
                if tail:
                    known.add(tail)
            except Exception as ex:
                print(f"    gagal daftar: {type(ex).__name__}: {str(ex)[:100]}")
                results.append((prof, False, "gagal daftar"))
        elif ok and args.out_dir:
            out = Path(args.out_dir) / f"cookie-{prof.replace(' ', '').lower()}.txt"
            out.parent.mkdir(parents=True, exist_ok=True)
            out.write_text(header, encoding="utf-8")
            print(f"    header ditulis ke {out}")
            results.append((prof, True, str(out)))
        else:
            results.append((prof, ok, detail))

    exe = chrome_exe()
    if exe and not args.keep_closed and not dedicated:
        subprocess.Popen([exe], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        print("\n  Chrome dinyalakan kembali (tanpa port debug)")

    print("\n  ringkasan:")
    for prof, ok, detail in results:
        print(f"    {prof:22s} {'OK ' if ok else '-- '} {detail}")
    return 0 if any(ok for _, ok, _ in results) else 2


def _kill_tree_all(name: str) -> None:
    subprocess.run(["taskkill", "/F", "/T", "/IM", name], capture_output=True, timeout=30)


# ── CLI ──────────────────────────────────────────────────────────────────────


def chrome_profile_identities() -> dict[str, dict]:
    """Nama + email tiap profil Chrome, dibaca dari `Local State`.

    Chrome menyimpan daftar profil beserta akun yang terakhir login di
    `profile.info_cache`. Ini satu-satunya sumber nama/email tanpa membuka
    database cookie, jadi aman dibaca walau Chrome sedang berjalan.
    """
    out: dict[str, dict] = {}
    state = Path(os.path.expandvars(
        r"%LOCALAPPDATA%\Google\Chrome\User Data\Local State"))
    if not state.exists():
        return out
    try:
        data = json.loads(state.read_text(encoding="utf-8", errors="replace"))
    except Exception:
        return out
    for key, info in (data.get("profile", {}).get("info_cache") or {}).items():
        out[str(key)] = {
            "name": str(info.get("name") or ""),
            "email": str(info.get("user_name") or ""),
        }
    return out


def cmd_list(_args) -> int:
    entries = discover()
    if not entries:
        print("  tidak ada profil browser dengan database cookie")
        return 1
    ident = chrome_profile_identities()
    print(f"  {len(entries)} profil ditemukan")
    for e in entries:
        cookies, problems = read_cookies(e)
        have = [n for n in REQUIRED if n in cookies]
        print(f"\n  {e['id']}")
        if e["id"].startswith("chrome:"):
            info = ident.get(e["id"].split(":", 1)[1], {})
            if info.get("name") or info.get("email"):
                print(f"    nama profil   : {info.get('name') or '-'}")
                print(f"    email google  : {info.get('email') or '-'}")
        print(f"    cookie google : {len(cookies)}")
        print(f"    syarat wajib  : {', '.join(have) if have else '(tidak ada)'}")
        if len(have) < len(REQUIRED):
            missing = [n for n in REQUIRED if n not in cookies]
            print(f"    kurang        : {', '.join(missing)}")
        for p in problems:
            print(f"    catatan       : {p}")
    return 0


def _pick(args) -> dict:
    entries = discover()
    if args.profile:
        for e in entries:
            if e["id"] == args.profile:
                return e
        raise SystemExit(f"profil {args.profile!r} tidak ditemukan; jalankan `list`")
    raise SystemExit("sebutkan --profile (lihat `list`) atau pakai --login-only")


def cmd_scan(args) -> int:
    """Pindai semua profil sekaligus: kelengkapan cookie + status login nyata.

    Ini perintah yang dipakai sehari-hari. Ia menjawab satu pertanyaan saja:
    profil mana yang benar-benar bisa dipakai NovaGemini sekarang. Profil yang
    DB-nya dikunci browser dilaporkan terpisah, bukan dianggap mati — bedanya
    penting, karena yang terkunci biasanya justru profil yang sedang dipakai.
    """
    entries = discover()
    live, dead, locked = [], [], []
    for e in entries:
        cookies, problems = read_cookies(e)
        if problems and not cookies:
            locked.append((e, problems[0]))
            continue
        have = [n for n in REQUIRED if n in cookies]
        if len(have) < len(REQUIRED):
            dead.append((e, f"cookie wajib kurang: {', '.join(n for n in REQUIRED if n not in cookies)}"))
            continue
        ok, detail = verify(build_header(cookies))
        (live if ok else dead).append((e, detail if not ok else f"{detail}, {len(cookies)} cookie"))

    print(f"  {len(entries)} profil dipindai\n")
    print(f"  HIDUP ({len(live)})")
    for e, d in live:
        print(f"    {e['id']:34s} {d}")
    if not live:
        print("    (tidak ada)")
    print(f"\n  MATI / TAMU ({len(dead)})")
    for e, d in dead:
        print(f"    {e['id']:34s} {d}")
    if not dead:
        print("    (tidak ada)")
    print(f"\n  TERKUNCI BROWSER ({len(locked)})")
    for e, d in locked:
        print(f"    {e['id']:34s} {d[:80]}")
    if not locked:
        print("    (tidak ada)")
    return 0 if live else 2


def list_accounts(base: str, api_key: str) -> list[dict]:
    req = urllib.request.Request(f"{base.rstrip('/')}/admin/api/cookies",
                                 headers={"Authorization": f"Bearer {api_key}"})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return json.loads(r.read().decode("utf-8", "replace")).get("items", [])
    except Exception:
        return []


def admin_patch(base: str, api_key: str, account_id: int, payload: dict) -> dict:
    req = urllib.request.Request(
        f"{base.rstrip('/')}/admin/api/cookies/{account_id}",
        data=json.dumps(payload).encode(), method="PATCH",
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {api_key}"},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read().decode("utf-8", "replace"))


def cmd_sync(args) -> int:
    """Perbarui cookie akun yang SUDAH terdaftar dari sumber yang Anda tentukan.

    Ini pasangan dari `import`. Bedanya: import menambah akun baru, sync menimpa
    cookie akun lama sehingga id, label, dan pengaturan proxy tetap. Dipakai
    untuk pola satu pemilik: browser memegang sesi, NovaGemini hanya memakai
    salinan segarnya, dan rotasi di NovaGemini dimatikan supaya keduanya tidak
    saling membatalkan token.
    """
    if args.user_data_dir:
        dedicated = Path(os.path.expandvars(args.user_data_dir))
        prof = args.profiles or "Default"
        print(f"  sumber: Chrome {prof} di {dedicated}")
        header, count, notes = harvest_chrome_profile(prof, dedicated, use_junction=False)
        for n in notes:
            print(f"    catatan: {n}")
    elif args.profile:
        entry = None
        for e in discover():
            if e["id"] == args.profile:
                entry = e
                break
        if not entry:
            raise SystemExit(f"profil {args.profile!r} tidak ditemukan; jalankan `list`")
        print(f"  sumber: {entry['id']}")
        cookies, problems = read_cookies(entry)
        for p in problems:
            print(f"    catatan: {p}")
        header, count = build_header(cookies), len(cookies)
    else:
        raise SystemExit("tentukan sumber: --user-data-dir <dir> atau --profile <id>")

    if not header:
        print("  tidak ada cookie dari sumber itu")
        return 2
    missing = [n for n in REQUIRED if f"{n}=" not in header]
    if missing:
        print(f"  cookie wajib kurang: {', '.join(missing)} — tidak disinkronkan")
        return 2

    ok, detail = verify(header)
    print(f"  {count} cookie — {'LOGIN' if ok else 'TIDAK LOGIN'} ({detail})")
    if not ok and not args.force:
        print("  sesi tidak hidup; tidak menimpa cookie akun yang ada (pakai --force untuk tetap)")
        return 2

    accounts = list_accounts(args.base, args.api_key)
    tail = sapisid_tail(header)
    target = None
    if args.account:
        target = next((a for a in accounts if int(a.get("id", 0)) == args.account), None)
    elif tail:
        same = [a for a in accounts if str(a.get("sapisid_tail") or "") == tail]
        # Beberapa akun bisa berbagi cookie yang sama (satu profil, banyak akun
        # Google). Tanpa nomor, "yang pertama" belum tentu yang dimaksud.
        if args.auth_user is not None:
            same = [a for a in same if int(a.get("auth_user") or 0) == args.auth_user]
        elif len(same) > 1:
            desc = ", ".join(
                "#%d %s (authuser=%d)" % (a["id"], a.get("label"), int(a.get("auth_user") or 0))
                for a in same)
            print(f"  {len(same)} akun berbagi SAPISID …{tail}: {desc}")
            print("  pilih salah satu dengan --account <id> atau --auth-user <n>")
            return 2
        target = same[0] if same else None
    if not target:
        print(f"  tidak ada akun terdaftar dengan SAPISID …{tail}. "
              f"Pakai `import` untuk menambah, atau --account <id>.")
        return 2

    payload: dict = {"cookie": header}
    if not args.keep_rotate:
        payload["rotate_disabled"] = True
    try:
        admin_patch(args.base, args.api_key, int(target["id"]), payload)
    except Exception as ex:
        print(f"  gagal memperbarui akun #{target['id']}: {type(ex).__name__}: {str(ex)[:120]}")
        return 2

    mode = "rotasi dimatikan (browser pemilik sesi)" if not args.keep_rotate else "rotasi tetap di NovaGemini"
    print(f"  akun #{target['id']} {target.get('label')!r} diperbarui — {mode}")
    return 0


def cmd_check(args) -> int:
    e = _pick(args)
    cookies, problems = read_cookies(e)
    header = build_header(cookies)
    ok, detail = verify(header)
    print(f"  {e['id']}: {'LOGIN' if ok else 'TIDAK LOGIN'} — {detail}")
    print(f"    cookie google: {len(cookies)} ({', '.join(sorted(cookies)[:8])}…)")
    for p in problems:
        print(f"    catatan: {p}")
    return 0 if ok else 2


def cmd_grab(args) -> int:
    e = _pick(args)
    cookies, problems = read_cookies(e)
    header = build_header(cookies)
    ok, detail = verify(header)
    print(f"  {e['id']}: {'LOGIN' if ok else 'TIDAK LOGIN'} — {detail}")
    print(f"  cookie: {len(cookies)} buah, header {len(header)} chars")
    for name in PREFERRED:
        if name in cookies:
            print(f"    {name:22s} {redact(cookies[name])}")
    for p in problems:
        print(f"  catatan: {p}")
    if args.out:
        out = Path(args.out)
        out.write_text(header, encoding="utf-8")
        try:
            os.chmod(out, 0o600)
        except Exception:
            pass
        print(f"  header ditulis ke {out} (izin terbatas)")
    return 0 if ok else 2


def cmd_import(args) -> int:
    entries = [e for e in discover() if not args.profile or e["id"] == args.profile]
    if not entries:
        raise SystemExit("tidak ada profil yang cocok")
    known = existing_tails(args.base, args.api_key)
    added = 0
    for e in entries:
        cookies, problems = read_cookies(e)
        if len([n for n in REQUIRED if n in cookies]) < len(REQUIRED):
            print(f"  {e['id']}: dilewati (cookie wajib tidak lengkap)")
            continue
        header = build_header(cookies)
        ok, detail = verify(header)
        if not ok:
            print(f"  {e['id']}: dilewati — {detail}")
            if args.login_only:
                continue
        tail = sapisid_tail(header)
        au = int(getattr(args, "auth_user", 0) or 0)
        key = f"{tail}@{au}" if tail else ""
        if key and key in known:
            print(f"  {e['id']}: sudah terdaftar (SAPISID …{tail} authuser={au}), tidak digandakan")
            continue
        label = args.label or e["id"].replace(":", "-")
        try:
            res = novagemini_add(args.base, args.api_key, label, header,
                                 f"cookie_grab dari {e['id']}", auth_user=au)
            print(f"  {e['id']}: didaftarkan sebagai #{res.get('id')} label={label!r} authuser={au}")
            added += 1
            if key:
                known.add(key)
        except Exception as ex:
            print(f"  {e['id']}: gagal daftar — {type(ex).__name__}: {str(ex)[:120]}")
        for p in problems:
            print(f"    catatan: {p}")
    print(f"\n  {added} akun ditambahkan")
    return 0 if added else 2


COOKIE_START_KEYS = ("SID=", "__Secure-1PSID=", "SAPISID=")


def parse_batch(path: Path) -> list[tuple[str, str]]:
    """Baca file batch: setiap entri = satu label + satu header Cookie utuh.

    Bentuk yang diterima (pemisah bebas: spasi, tab, titik dua, pipe):

        yahukimo2   SID=...; HSID=...; SAPISID=...
        yahukimo3:  __Secure-1PSID=...; ...
        # baris komentar diabaikan

    Header yang terpotong ke beberapa baris disambung otomatis: baris lanjutan
    adalah baris yang tidak memuat salah satu nama cookie awal.
    """
    entries: list[tuple[str, str]] = []
    label = ""
    buf = ""
    expect_cookie = False

    def flush_if_buf() -> None:
        nonlocal label, buf
        if buf.strip():
            entries.append((label.strip(), " ".join(buf.split())))
            label, buf = "", ""

    for raw in path.read_text(encoding="utf-8-sig", errors="replace").splitlines():
        line = raw.strip()
        if not line:
            flush_if_buf()
            continue
        if line.startswith("#"):
            continue
        cut = [line.find(k) for k in COOKIE_START_KEYS if k in line]
        if not cut and line.lower().startswith("cookie:"):
            # Tempelan DevTools: baris "cookie:" lalu nilainya di baris berikutnya.
            flush_if_buf()
            rest = line.split(":", 1)[1].strip()
            if rest:
                buf = rest
            else:
                expect_cookie = True
            continue
        if expect_cookie:
            buf = line
            expect_cookie = False
            continue
        if cut:
            at = min(cut)
            head = line[:at].strip(" :|\t-—=")
            # "cookie: SID=…" pada tempelan header bukan nama akun.
            if head and head.lower() not in ("cookie", "set-cookie"):
                flush_if_buf()
                label = head
            buf = line[at:]
        elif buf:
            buf += " " + line
        elif label and re.match(r"^[A-Za-z][A-Za-z-]*:\s*\S*$", line):
            # Header lain dari tempelan DevTools (accept, user-agent, …): abaikan.
            continue
        else:
            label = line.strip(" :|\t-—=")
    flush_if_buf()
    return entries


def cmd_batch(args) -> int:
    """Daftarkan banyak akun sekaligus dari file teks.

    Dipakai saat operator menyerahkan belasan sesi dalam satu berkas: verifikasi
    dijalankan paralel (tanpa memakai kuota model), lalu tiap sesi yang hidup
    didaftarkan sebagai akun baru — atau menimpa akun lama kalau SAPISID-nya
    sudah terdaftar, supaya tidak lahir kembaran.
    """
    from concurrent.futures import ThreadPoolExecutor

    src = Path(os.path.expandvars(args.file))
    files = sorted(src.glob("*.txt")) if src.is_dir() else [src]
    files = [f for f in files if f.is_file()]
    if not files:
        print(f"  tidak ada file di {src}")
        return 2

    entries: list[tuple[str, str]] = []
    for f in files:
        got = parse_batch(f)
        print(f"  {f.name}: {len(got)} entri")
        entries += got
    if not entries:
        print("  berkas tidak berisi entri label+cookie")
        return 2

    print(f"\n  memeriksa {len(entries)} sesi (paralel 4)…")
    with ThreadPoolExecutor(max_workers=4) as pool:
        verdicts = list(pool.map(lambda e: verify(e[1]), entries))

    accounts = list_accounts(args.base, args.api_key)
    by_tail = {f"{a.get('sapisid_tail')}@{int(a.get('auth_user') or 0)}": a
               for a in accounts if a.get("sapisid_tail")}
    print()
    seen: set[str] = set()
    added = updated = skipped = 0
    for (label, header), (ok, detail) in zip(entries, verdicts):
        tail = sapisid_tail(header)
        # `nama@3` = akun indeks 3 di profil yang sama; `--auth-user` jadi nilai
        # bawaan untuk entri yang tidak menuliskan nomor.
        au = int(args.auth_user or 0)
        name = label
        if "@" in name:
            head, _, suffix = name.rpartition("@")
            if suffix.strip().isdigit():
                name, au = head.strip(), int(suffix.strip())
        name = name or (f"gemini-{tail}" if tail else "gemini")
        key = f"{tail}@{au}" if tail else ""
        missing = [n for n in REQUIRED if f"{n}=" not in header]
        if missing:
            print(f"  {name:22s} LEWAT  cookie kurang: {', '.join(missing)}")
            skipped += 1
            continue
        if not ok and not args.force:
            print(f"  {name:22s} LEWAT  {detail}")
            skipped += 1
            continue
        if key and key in seen:
            print(f"  {name:22s} LEWAT  duplikat di batch ini (SAPISID …{tail} authuser={au})")
            skipped += 1
            continue
        if key:
            seen.add(key)
        existing = by_tail.get(key) if key else None
        try:
            if existing and not args.add_duplicates:
                payload: dict = {"cookie": header}
                if not args.keep_rotate:
                    payload["rotate_disabled"] = True
                admin_patch(args.base, args.api_key, int(existing["id"]), payload)
                print(f"  {name:22s} PERBARUI #{existing['id']} ({existing.get('label')}) — {detail}")
                updated += 1
            else:
                res = novagemini_add(args.base, args.api_key, name, header,
                                     "cookie_grab batch", auth_user=au)
                new_id = res.get("id")
                if new_id and not args.keep_rotate:
                    admin_patch(args.base, args.api_key, int(new_id), {"rotate_disabled": True})
                print(f"  {name:22s} BARU #{new_id} authuser={au} — {detail}")
                added += 1
        except Exception as ex:
            print(f"  {name:22s} GAGAL  {type(ex).__name__}: {str(ex)[:100]}")
            skipped += 1

    print(f"\n  hasil: {added} baru, {updated} diperbarui, {skipped} dilewati")
    return 0 if (added or updated) else 2


def enumerate_profile_accounts(header: str, limit: int = 10) -> list[tuple[int, str]]:
    """Temukan semua akun Google di satu set cookie (multi-login).

    Satu profil browser bisa memegang banyak akun sekaligus dengan SATU set
    cookie; Google memilih akun lewat path `/u/N/app`. Fungsi ini menelusuri
    N = 0,1,2,… dan membaca email yang benar-benar disajikan halaman untuk
    nomor itu, lalu berhenti saat nomornya tidak lagi menjawab akun.
    """
    found: list[tuple[int, str]] = []
    for n in range(limit):
        url = "https://gemini.google.com/app" if n == 0 else f"https://gemini.google.com/u/{n}/app"
        req = urllib.request.Request(url, headers={
            "Cookie": strip_marker(header), "User-Agent": UA,
            "Accept": "text/html", "Accept-Language": "en-US,en;q=0.9",
            "Accept-Encoding": "identity"})
        try:
            with urllib.request.urlopen(req, timeout=45) as r:
                body = r.read().decode("utf-8", "replace")
        except Exception:
            break
        if "SNlM0e" not in body:
            break
        mails = [m.lower() for m in re.findall(r"[A-Za-z0-9._%+-]+@gmail\.com", body)]
        if not mails:
            break
        # Email akun muncul paling sering di bootstrap halaman; pemilik sesi
        # adalah yang paling banyak disebut.
        email = max(set(mails), key=mails.count)
        if any(email == e for _n, e in found):
            break
        found.append((n, email))
    return found


def strip_marker(header: str) -> str:
    return "; ".join(p for p in header.split(";") if not p.strip().startswith("__NOVA_AU="))


def cmd_accounts(args) -> int:
    """Daftarkan SEMUA akun dari profil yang dipilih, satu baris per akun.

    Ini jalur yang dipakai kalau operator sudah login beberapa akun di satu
    profil browser: satu perintah, seluruh akun terdaftar dengan nomor
    `authuser` yang benar.
    """
    entries = [e for e in discover() if not args.profile or e["id"] == args.profile]
    if not entries:
        raise SystemExit("tidak ada profil yang cocok")
    known = existing_tails(args.base, args.api_key)
    added = skipped = 0
    for e in entries:
        cookies, problems = read_cookies(e)
        if len([n for n in REQUIRED if n in cookies]) < len(REQUIRED):
            print(f"  {e['id']}: dilewati (cookie wajib tidak lengkap)")
            continue
        header = build_header(cookies)
        ok, detail = verify(header)
        print(f"\n  {e['id']}: {detail}")
        if not ok:
            skipped += 1
            continue
        found = enumerate_profile_accounts(header, limit=args.limit)
        if not found:
            print("    tidak ada akun yang bisa dibaca dari halaman")
            skipped += 1
            continue
        tail = sapisid_tail(header)
        for n, email in found:
            label = email.split("@")[0]
            key = f"{tail}@{n}"
            if key in known:
                print(f"    [{n}] {email:38s} sudah terdaftar")
                skipped += 1
                continue
            try:
                res = novagemini_add(args.base, args.api_key, label, header,
                                     f"cookie_grab accounts dari {e['id']}", auth_user=n)
                print(f"    [{n}] {email:38s} -> #{res.get('id')} label={label!r}")
                known.add(key)
                added += 1
            except Exception as ex:
                print(f"    [{n}] {email:38s} GAGAL {type(ex).__name__}: {str(ex)[:100]}")
                skipped += 1
        for p in problems:
            print(f"    catatan: {p}")
    print(f"\n  hasil: {added} akun baru, {skipped} dilewati")
    return 0 if added else 2


def cmd_refresh(args) -> int:
    """Segarkan cookie SEMUA akun yang memakai sesi dari profil yang sama.

    Satu profil browser bisa melayani belasan akun dengan satu set cookie; kalau
    sesi itu berputar di browser, seluruh akun itu mati bersamaan. Perintah ini
    membaca ulang profil lalu menimpa cookie tiap akun yang SAPISID-nya cocok,
    tanpa menyentuh nomor authuser maupun label.
    """
    entries = [e for e in discover() if not args.profile or e["id"] == args.profile]
    if not entries:
        raise SystemExit("tidak ada profil yang cocok")
    refreshed = skipped = 0
    for e in entries:
        cookies, problems = read_cookies(e)
        if len([n for n in REQUIRED if n in cookies]) < len(REQUIRED):
            print(f"  {e['id']}: dilewati (cookie wajib tidak lengkap)")
            skipped += 1
            continue
        header = build_header(cookies)
        ok, detail = verify(header)
        print(f"\n  {e['id']}: {detail}")
        if not ok and not args.force:
            print("    sesi sumber tidak hidup; akun tidak disentuh (pakai --force untuk tetap menimpa)")
            skipped += 1
            continue
        tail = sapisid_tail(header)
        hits = [a for a in list_accounts(args.base, args.api_key)
                if tail and str(a.get("sapisid_tail") or "") == tail]
        if not hits:
            print(f"    tidak ada akun terdaftar dengan SAPISID …{tail} (pakai `accounts` untuk mendaftarkan)")
            skipped += 1
            continue
        for a in hits:
            try:
                admin_patch(args.base, args.api_key, int(a["id"]), {"cookie": header, "status": "enabled"})
                print(f"    #{a['id']} {str(a.get('label'))[:26]:26s} authuser={int(a.get('auth_user') or 0)} disegarkan")
                refreshed += 1
            except Exception as ex:
                print(f"    #{a['id']} GAGAL {type(ex).__name__}: {str(ex)[:100]}")
                skipped += 1
        for p in problems:
            print(f"    catatan: {p}")
    print(f"\n  hasil: {refreshed} akun disegarkan, {skipped} dilewati")
    return 0 if refreshed else 2


def main() -> int:
    ap = argparse.ArgumentParser(description="Ambil cookie sesi Google dari browser lokal")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p_list = sub.add_parser("list", help="daftar profil + kelengkapan cookie")
    p_list.set_defaults(func=cmd_list)

    p_scan = sub.add_parser("scan", help="pindai semua profil: mana yang benar-benar login")
    p_scan.set_defaults(func=cmd_scan)

    p_sync = sub.add_parser("sync", help="perbarui cookie akun yang sudah terdaftar dari sumber")
    p_sync.add_argument("--user-data-dir", help="folder profil Chrome khusus")
    p_sync.add_argument("--profiles", help="nama profil di dalam folder itu (default: Default)")
    p_sync.add_argument("--profile", help="profil berbasis file, mis. firefox:s1d1gj83.default-release")
    p_sync.add_argument("--account", type=int, help="id akun tujuan (kosong = cocokkan lewat SAPISID)")
    p_sync.add_argument("--auth-user", type=int, default=None,
                        help="kalau beberapa akun berbagi cookie yang sama, pilih yang nomornya ini")
    p_sync.add_argument("--api-key", required=True)
    p_sync.add_argument("--base", default="http://127.0.0.1:8083")
    p_sync.add_argument("--keep-rotate", action="store_true",
                        help="jangan matikan rotasi NovaGemini untuk akun ini")
    p_sync.add_argument("--force", action="store_true",
                        help="tetap timpa walaupun sesi sumber tidak lolos verifikasi")
    p_sync.set_defaults(func=cmd_sync)

    p_chr = sub.add_parser("chrome",
                           help="ambil cookie Chrome lewat CDP (satu-satunya jalur untuk v20)")
    p_chr.add_argument("--profiles", help='daftar profil dipisah koma, mis. "Default,Profile 2"')
    p_chr.add_argument("--user-data-dir",
                       help="folder profil khusus (bukan folder Chrome default); boleh berjalan "
                            "berdampingan dengan browser operator")
    p_chr.add_argument("--login", action="store_true",
                       help="buka Chrome pada folder profil khusus untuk login manual")
    p_chr.add_argument("--api-key", help="kunci API NovaGemini; kalau ada, akun langsung didaftarkan")
    p_chr.add_argument("--base", default="http://127.0.0.1:8083")
    p_chr.add_argument("--label", help="label akun (hanya untuk satu profil)")
    p_chr.add_argument("--out-dir", help="tulis header ke folder ini kalau --api-key tidak diberi")
    p_chr.add_argument("--force", action="store_true", help="jangan tanya walau Chrome sedang jalan")
    p_chr.add_argument("--keep-closed", action="store_true", help="jangan nyalakan Chrome di akhir")
    p_chr.set_defaults(func=cmd_chrome)

    p_check = sub.add_parser("check", help="verifikasi sesi satu profil")
    p_check.add_argument("--profile", required=True)
    p_check.set_defaults(func=cmd_check)

    p_grab = sub.add_parser("grab", help="ambil header cookie (redacted) dan tulis ke file")
    p_grab.add_argument("--profile", required=True)
    p_grab.add_argument("--out", help="path file untuk header lengkap")
    p_grab.set_defaults(func=cmd_grab)

    p_imp = sub.add_parser("import", help="verifikasi lalu daftarkan ke NovaGemini")
    p_imp.add_argument("--profile", help="profil tertentu; kosong = semua")
    p_imp.add_argument("--login-only", action="store_true",
                       help="lewati profil yang sesinya tidak hidup")
    p_imp.add_argument("--label", help="label akun di NovaGemini")
    p_imp.add_argument("--auth-user", type=int, default=0,
                       help="nomor akun di profil itu (Google multi-login, 0 = akun utama)")
    p_imp.add_argument("--base", default="http://127.0.0.1:8083")
    p_imp.add_argument("--api-key", required=True)
    p_imp.set_defaults(func=cmd_import)

    p_bat = sub.add_parser("batch",
                           help="impor banyak akun dari file: label + header Cookie per entri")
    p_bat.add_argument("file", help="berkas .txt, atau folder berisi .txt")
    p_bat.add_argument("--base", default="http://127.0.0.1:8083")
    p_bat.add_argument("--api-key", required=True)
    p_bat.add_argument("--auth-user", type=int, default=0,
                       help="nomor akun bawaan; entri bisa menimpanya dengan `nama@3`")
    p_bat.add_argument("--force", action="store_true",
                       help="tetap daftarkan walau verifikasi sesi tidak lolos")
    p_bat.add_argument("--keep-rotate", action="store_true",
                       help="jangan matikan rotasi NovaGemini untuk akun hasil impor")
    p_bat.add_argument("--add-duplicates", action="store_true",
                       help="tambahkan akun baru walau SAPISID-nya sudah terdaftar")
    p_bat.set_defaults(func=cmd_batch)

    p_acc = sub.add_parser("accounts",
                           help="daftarkan semua akun Google di satu profil (multi-login)")
    p_acc.add_argument("--profile", help="profil tertentu; kosong = semua")
    p_acc.add_argument("--limit", type=int, default=10,
                       help="batas nomor akun yang diperiksa (default 10)")
    p_acc.add_argument("--base", default="http://127.0.0.1:8083")
    p_acc.add_argument("--api-key", required=True)
    p_acc.set_defaults(func=cmd_accounts)

    p_ref = sub.add_parser("refresh",
                           help="segarkan cookie semua akun yang berbagi sesi satu profil")
    p_ref.add_argument("--profile", help="profil tertentu; kosong = semua")
    p_ref.add_argument("--base", default="http://127.0.0.1:8083")
    p_ref.add_argument("--api-key", required=True)
    p_ref.add_argument("--force", action="store_true",
                       help="tetap timpa walau sesi sumber tidak lolos verifikasi")
    p_ref.set_defaults(func=cmd_refresh)

    args = ap.parse_args()
    return args.func(args)


if __name__ == "__main__":
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    raise SystemExit(main())
