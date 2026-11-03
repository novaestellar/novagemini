@echo off
rem novagemini - resync cookie dari profil Firefox ke server (dijadwalkan tiap 30 menit)
set PYTHONIOENCODING=utf-8
rem kunci TIDAK disimpan di repo: skrip mengambilnya dari panel (loopback, tanpa auth)
cd /d D:\Labs\novagemini
echo [%date% %time%] refresh >> data\refresh.log
python tools\cookie_grab.py refresh --profile "firefox:s1d1gj83.default-release" >> data\refresh.log 2>&1
python tools\cookie_grab.py refresh --profile "firefox:vRM15uzb.Profile 1" >> data\refresh.log 2>&1
