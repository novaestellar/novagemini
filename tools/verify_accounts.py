#!/usr/bin/env python3
"""verify_accounts.py — uji setiap akun di pool dengan cookie yang TERSIMPAN.

Kenapa perlu: panel menampilkan status dari database (hasil request terakhir),
sedangkan yang menentukan hidup/mati adalah jawaban Google atas /app dengan
cookie yang benar-benar kita kirim. Skrip ini menguji hal terakhir itu, satu
akun satu request, dan mencetak hasilnya apa adanya.

Pemakaian:
    python tools/verify_accounts.py
    python tools/verify_accounts.py --ids 7,8,10
"""

import argparse
import sqlite3
import ssl
import sys
import urllib.error
import urllib.request

DB = r"D:\Labs\novagemini\data\gemini.db"
UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def load_accounts(ids=None):
    conn = sqlite3.connect(f"file:{DB}?mode=ro", uri=True)
    cols = [r[1] for r in conn.execute("pragma table_info(accounts)")]
    q = "select * from accounts order by id"
    rows = [dict(zip(cols, r)) for r in conn.execute(q)]
    conn.close()
    if ids:
        rows = [r for r in rows if r["id"] in ids]
    return rows


def probe(cookie, auth_user):
    plain = "; ".join(p for p in cookie.split("; ") if not p.startswith("__NOVA_AU="))
    path = "/app" if auth_user in (0, None) else f"/u/{auth_user}/app"
    req = urllib.request.Request(
        "https://gemini.google.com" + path,
        headers={"Cookie": plain, "User-Agent": UA,
                 "Accept": "text/html,application/xhtml+xml"})
    op = urllib.request.build_opener(NoRedirect)
    try:
        r = op.open(req, timeout=25)
        body = r.read(4000).decode("utf-8", "replace")
        return r.status, "", ("SNlM0e" in body)
    except urllib.error.HTTPError as e:
        return e.code, e.headers.get("Location", ""), False
    except Exception as e:  # noqa: BLE001
        return 0, f"{type(e).__name__}: {e}", False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ids", default="", help="mis. 7,8,10 (kosong = semua)")
    args = ap.parse_args()
    ids = {int(x) for x in args.ids.split(",") if x.strip().isdigit()} or None

    rows = load_accounts(ids)
    if not rows:
        print("  tidak ada akun di database")
        return 1

    alive = 0
    print("  id  label                    au  hasil")
    for d in rows:
        status, loc, ok = probe(d["cookie"], d["auth_user"])
        if status == 200 and ok:
            alive += 1
            verdict = "HIDUP (SNlM0e ada)"
        elif status == 200:
            verdict = "200 tapi tanpa SNlM0e (halaman login/syarat)"
        elif status in (301, 302, 303, 307, 308):
            verdict = f"{status} -> {loc[:52]}"
        else:
            verdict = f"{status} {loc[:52]}"
        print(f"  {d['id']:>3} {str(d['label'])[:24]:24s} {d['auth_user']:>2}  {verdict}")

    print(f"\n  ringkas: {alive}/{len(rows)} akun benar-benar hidup dengan cookie tersimpan")
    return 0 if alive else 2


if __name__ == "__main__":
    sys.exit(main())
