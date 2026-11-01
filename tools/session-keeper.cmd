@echo off
rem novagemini - jaga sesi Gemini tetap hidup (jalankan tiap 6 jam)
set PYTHONIOENCODING=utf-8
set KEY=***REMOVED-APIKEY***
cd /d D:\Labs\novagemini
echo [%date% %time%] keeper >> data\keeper.log
python tools\session_keeper.py --api-key %KEY% >> data\keeper.log 2>&1