@echo off
rem novagemini - jaga sesi Gemini tetap hidup (jalankan tiap 6 jam)
set PYTHONIOENCODING=utf-8
rem kunci TIDAK disimpan di repo: skrip mengambilnya dari panel (loopback, tanpa auth)
cd /d D:\Labs\novagemini
echo [%date% %time%] keeper >> data\keeper.log
python tools\session_keeper.py >> data\keeper.log 2>&1
