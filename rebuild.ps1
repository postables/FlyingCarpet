del '.\bin\flyingcarpet (Windows CLI).zip'

Copy-Item .\WFD_DLL\x64\Release\WFD_DLL.dll .\static\wfd.dll
go-bindata -o static.go static\

Copy-Item .\icons\Windows\fc.syso .

go build -o .\flyingcarpet.exe

Compress-Archive -Path '.\flyingcarpet.exe' -DestinationPath '.\bin\flyingcarpet (Windows CLI).zip'

del .\fc.syso
