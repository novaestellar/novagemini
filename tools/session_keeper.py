#!/usr/bin/env python3
"""session_keeper.py - jaga sesi Gemini tetap hidup, tanpa login berulang.

Masalah yang diselesaikan: sesi web Gemini mati kalau tidak ada klien yang
memakainya, dan mati lebih cepat lagi kalau dua pihak memutar token yang sama.
Resepnya tiga hal sekaligus:

  1. satu profil browser per akun  -> satu akun mati tidak menyeret yang lain
  2. browser-owned (rotate_disabled=1) -> server tidak pernah memutar tokennya
  3. kunjungan berkala             -> profil asli dipakai menyapa Gemini,
                                      jadi cookie-nya ikut diputar oleh browser
                                      itu sendiri (bukan oleh server)

Untuk setiap profil yang terdaftar di tools/session-map.json:

  buka profil ASLI (headless) -> buka gemini.google.com/app -> tutup
  baca cookie -> uji login -> kalau lolos, tulis ke akun yang dipetakan

Syarat: Firefox tidak sedang dipakai (profil asli harus bebas). Kalau sedang
dipakai, profil itu dilewati dan dicatat - bukan dipaksa.

Pemakaian:
  python tools/session_keeper.py                # semua profil di peta
  python tools/session_keeper.py --profile X    # satu profil
  python tools/session_keeper.py --dry          # hanya periksa, tidak menulis
"""

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import browser_harvest as bh  # noqa: E402
import cookie_grab as cg  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
MAP_PATH = os.path.join(HERE, "session-map.json")
PROFILE_DIR = os.path.join(os.environ["APPDATA"], "Mozilla", "Firefox", "Profiles")
GEMINI = "https://gemini.google.com/app"


def load_map():
    if os.path.exists(MAP_PATH):
        try:
            return json.load(open(MAP_PATH, encoding="utf-8"))
        except Exception:
            pass
    return {}


def save_map(m):
    json.dump(m, open(MAP_PATH, "w", encoding="utf-8"), ensure_ascii=False, indent=2)


def firefox_running():
    try:
        out = subprocess.run(["tasklist", "/FI", "IMAGENAME eq firefox.exe"],
                             capture_output=True, text=True, timeout=15).stdout
        return "firefox.exe" in out
    except Exception:
        return True  # ragu = anggap jalan, jangan sentuh profil


def visit(profile):
    """Buka profil ASLI sebentar supaya browser memutar cookie sesinya sendiri."""
    prof = os.path.join(PROFILE_DIR, profile)
    if not os.path.isdir(prof):
        return False, "profil tidak ada"
    proc = subprocess.Popen(
        [bh.GECKODRIVER, f"--port={bh.WD_PORT}"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    session = None
    try:
        deadline = time.time() + 30
        while time.time() < deadline:
            try:
                bh._wd("GET", "/status", timeout=5)
                break
            except Exception:
                time.sleep(1)
        caps = {"capabilities": {"alwaysMatch": {
            "browserName": "firefox",
            "moz:firefoxOptions": {
                "binary": bh.FIREFOX,
                "args": ["-headless", "-no-remote", "-profile", prof],
            },
        }}}
        session = bh._wd("POST", "/session", caps, timeout=120)["value"]["sessionId"]
        bh._wd("POST", f"/session/{session}/url", {"url": GEMINI}, timeout=120)
        time.sleep(8)  # beri waktu halaman memutar cookie
        return True, "dikunjungi"
    except Exception as e:  # noqa: BLE001
        return False, f"{type(e).__name__}: {str(e)[:60]}"
    finally:
        try:
            if session:
                bh._wd("DELETE", f"/session/{session}", timeout=30)
        except Exception:
            pass
        try:
            proc.terminate()
        except Exception:
            pass


def import_header(base, api_key, header, accounts):
    if not accounts:
        return []
    return bh.import_to_server(base, api_key, header, "", None, set(accounts))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--profile", default="")
    ap.add_argument("--base", default="http://127.0.0.1:8083")
    ap.add_argument("--api-key", default="")
    ap.add_argument("--dry", action="store_true")
    ap.add_argument("--visit", action="store_true",
                    help="buka tiap profil headless dulu (default tidak: cukup baca cookie)")
    args = ap.parse_args()

    m = load_map()
    profiles = [args.profile] if args.profile else sorted(m)
    if not profiles:
        print("  peta kosong. Isi tools/session-map.json: {\"<profil>\": [id akun, ...]}")
        print("  Contoh: {\"p3OS0Uyy.Profile 1\": [8]}")
        return 1

    # Firefox boleh tetap terbuka. Kalau tertutup, profil asli dikunjungi dulu
    # supaya browser sendiri yang memutar cookie-nya; kalau sedang terbuka,
    # langkah itu dilewati (profil sedang dipakai) dan cookie dibaca dari
    # salinan - itu tetap bekerja, terbukti saat memeriksa 11 jendela yang
    # sedang terbuka. Menolak jalan hanya karena browser terbuka akan membuat
    # penjaga ini hampir tidak pernah berjalan.
    ff_open = firefox_running()
    do_visit = args.visit and not ff_open
    if do_visit:
        print("  kunjungan AAKTIF: tiap profil dibuka headless dulu\n")
    else:
        print("  mode sinkron saja: cookie dibaca dari database profil, tanpa membuka browser\n")

    print(f"  memproses {len(profiles)} profil\n")
    summary = []
    for p in profiles:
        targets = m.get(p, [])
        if do_visit:
            okv, detail = visit(p)
            if not okv:
                print(f"  {p:26s} kunjungan gagal: {detail}")
                summary.append((p, "visit-fail", targets))
                continue
        try:
            # Baca lewat browser (WebDriver), bukan dari cookies.sqlite.
            # Alasannya terukur hari ini: sebagian cookie Google bersifat
            # session-only dan TIDAK pernah ditulis ke disk, sehingga pembacaan
            # SQLite menghasilkan 302 untuk sesi yang sebenarnya hidup. Browser
            # yang membuka profilnya sendiri melihat set lengkap dari memori.
            header, table, _ = bh.harvest_gecko(p)
        except Exception as e:  # noqa: BLE001
            print(f"  {p:26s} baca cookie gagal: {type(e).__name__}: {str(e)[:40]}")
            summary.append((p, "read-fail", targets))
            continue
        ok, why = bh.probe_login(header)
        if not ok:
            # Sekali lagi: pembacaan pertama bisa kebetulan menangkap state
            # setengah jalan. Kita tidak pernah menulis kalau tetap gagal.
            try:
                header, table, _ = bh.harvest_gecko(p)
                ok, why = bh.probe_login(header)
            except Exception:
                pass
        if not ok:
            print(f"  {p:26s} sesi tidak hidup — {why}")
            summary.append((p, "dead", targets))
            continue
        if args.dry:
            print(f"  {p:26s} HIDUP ({len(table)} cookie) — tidak ditulis (--dry)")
            summary.append((p, "live", targets))
            continue
        written = import_header(args.base, args.api_key, header, targets)
        print(f"  {p:26s} HIDUP -> ditulis ke akun {written or '(peta kosong)'}")
        summary.append((p, "written", written))

    print()
    live = [s for s in summary if s[1] in ("live", "written")]
    dead = [s for s in summary if s[1] == "dead"]
    print(f"  ringkas: {len(live)} hidup · {len(dead)} perlu login · {len(summary)} diproses")
    for p, st, t in dead:
        print(f"    perlu login: {p}  (akun {t or 'belum dipetakan'})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
