@echo off
rem novagemini - resync cookie dari profil Firefox ke server (dijadwalkan tiap 30 menit)
set PYTHONIOENCODING=utf-8
set KEY=***REMOVED-APIKEY***
cd /d D:\Labs\novagemini
echo [%date% %time%] refresh >> data\refresh.log
python tools\cookie_grab.py refresh --profile "firefox:s1d1gj83.default-release" --api-key %KEY% >> data\refresh.log 2>&1
python tools\cookie_grab.py refresh --profile "firefox:vRM15uzb.Profile 1" --api-key %KEY% >> data\refresh.log 2>&1