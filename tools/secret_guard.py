#!/usr/bin/env python3
"""secret_guard.py - cegat rahasia sebelum masuk commit.

Dipasang sebagai .git/hooks/pre-commit. Memeriksa berkas yang SEDANG di-stage
(bukan seluruh working tree), lalu menolak commit kalau menemukan:

  - API key novagemini      sk-gemini-<hex>
  - kunci klien             sk-gm-<hex>
  - kunci 9router           wzr_live_<...>
  - token GitHub/Notion     github_pat_… / ghp_… / ntn_…
  - cookie sesi Google      SID=…, SAPISID=…, __Secure-1PSID=…, g.a000…
  - berkas state lokal      tools/session-map.json, tools/firefox-profiles.json

Pemasangan (sekali per klon):
    python tools/secret_guard.py --install
Pemeriksaan manual:
    python tools/secret_guard.py
"""

import argparse
import os
import re
import subprocess
import sys

# Pola yang selalu ditolak, di berkas apa pun.
PATTERNS = [
    ("API key novagemini", re.compile(r"sk-gemini-[0-9a-f]{8,}")),
    ("kunci klien", re.compile(r"sk-gm-[0-9a-f]{8,}")),
    ("kunci 9router", re.compile(r"wzr_live_[A-Za-z0-9]{6,}")),
    ("token GitHub", re.compile(r"(github_pat_|ghp_|gho_)[A-Za-z0-9_]{10,}")),
    ("token Notion", re.compile(r"ntn_[A-Za-z0-9]{10,}")),
    ("cookie sesi Google", re.compile(r"(SID|HSID|SAPISID|__Secure-1PSID|__Secure-3PSID)=[A-Za-z0-9_\-]{12,}")),
    ("nilai SID Google", re.compile(r"g\.a000[A-Za-z0-9_\-]{8,}")),
]

# Berkas yang isinya keadaan mesin ini (nama akun, pemetaan profil).
LOCAL_STATE = ("tools/session-map.json", "tools/firefox-profiles.json")


def staged_files():
    out = subprocess.run(["git", "diff", "--cached", "--name-only", "--diff-filter=ACMR"],
                         capture_output=True, text=True, encoding="utf-8")
    return [f.strip() for f in out.stdout.splitlines() if f.strip()]


def scan(files):
    findings = []
    for f in files:
        if f in LOCAL_STATE:
            findings.append((f, 0, "berkas state lokal (nama akun)", ""))
            continue
        try:
            with open(f, encoding="utf-8", errors="ignore") as fh:
                for n, line in enumerate(fh, 1):
                    for label, pat in PATTERNS:
                        m = pat.search(line)
                        if m:
                            # tampilkan hanya potongan awal, jangan bocorkan nilainya
                            findings.append((f, n, label, m.group(0)[:12] + "…"))
        except (OSError, IsADirectoryError):
            continue
    return findings


def install():
    git_dir = subprocess.run(["git", "rev-parse", "--git-dir"], capture_output=True,
                             text=True, encoding="utf-8").stdout.strip() or ".git"
    hook = os.path.join(git_dir, "hooks", "pre-commit")
    os.makedirs(os.path.dirname(hook), exist_ok=True)
    body = ('#!/bin/sh\n'
            '# dipasang oleh tools/secret_guard.py - menolak commit yang memuat rahasia\n'
            'python "{}" "$@"\n').format(os.path.abspath(__file__).replace("\\", "/"))
    with open(hook, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(body)
    try:
        os.chmod(hook, 0o755)
    except OSError:
        pass
    print(f"  terpasang: {hook}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--install", action="store_true", help="pasang sebagai .git/hooks/pre-commit")
    args = ap.parse_args()
    if args.install:
        install()
        return 0

    files = staged_files()
    if not files:
        return 0
    findings = scan(files)
    if not findings:
        return 0

    print("\n  COMMIT DITOLAK — berkas berikut memuat rahasia/state lokal:\n")
    for f, n, label, sample in findings:
        where = f"baris {n}" if n else "berkas"
        print(f"    {f}  ({where})  -> {label}  {sample}")
    print("\n  Perbaiki dulu: pindahkan nilainya ke service.json (env) atau ke panel,")
    print("  dan pastikan berkas state lokal tidak ikut di-stage.\n")
    return 1


if __name__ == "__main__":
    sys.exit(main())
