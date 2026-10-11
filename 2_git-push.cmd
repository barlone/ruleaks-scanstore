for /F "delims== tokens=1,* eol=#" %%i in (gittoken) do set %%i=%%~j

git push https://barlone:%GITTOKEN%@github.com/barlone/dsstalker-api-gate.git
