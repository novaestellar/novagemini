' novagemini - start silently at logon, logs to data\srv.log and data\srv.err
Set sh = CreateObject("WScript.Shell")
sh.CurrentDirectory = "D:\Labs\novagemini"
sh.Run "cmd /c ""D:\Labs\novagemini\novagemini.exe"" 1>> ""D:\Labs\novagemini\data\srv.log"" 2>> ""D:\Labs\novagemini\data\srv.err""", 0, False