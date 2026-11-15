@echo off
rem Starts bareloopd on this folder's config.json. The daemon must run with this folder
rem as its working directory - every path in config.json and vroot.json is relative to
rem it - which the cd below guarantees however you launch this bat.
rem
rem This bat starts the daemon; start-chrome.bat starts the browser it drives. Either
rem order works: the browser MCP attaches lazily, on the first cdp_* call.
rem Kept runnable on LF or CRLF line endings alike: ASCII only, no labels, no goto,
rem no parenthesized blocks, no chained - every line here stands alone.
setlocal
cd /d "%~dp0"
if exist "bin\bareloopd.exe" set "HAVE=1"
if not defined HAVE echo bin\bareloopd.exe is missing. Build it into this folder first, from the repo root:
if not defined HAVE echo     go build -o samples\personal-agent-win\bin\bareloopd.exe .\cmd\bareloopd
if not defined HAVE echo bin\ also wants chrome-mcp.exe and mcp-vroot.exe - see the mcp block in config.json
if not defined HAVE pause
if not defined HAVE exit /b 1
rem The agent's file tools are jailed to workspace, so it cannot learn where that folder
rem lives on this machine. Write the path down for it: the systemPrompt tells it to read
rem /HERE.txt and build file:/// URLs from it. This is what keeps the folder copyable.
rem Note the full path - %~dp0 alone is the bat's folder, one level above the workspace.
>"%~dp0workspace\HERE.txt" echo %~dp0workspace
bin\bareloopd.exe -config config.json
pause
