@echo off
rem Starts the Chrome this agent drives: CDP on 9223 - the -cdp address in config.json.
rem Profile folder: the profile directory next to this bat. --user-data-dir needs an
rem absolute path; %~dp0 is this folder, so the profile travels with this copy.
rem Kept runnable on LF or CRLF line endings alike: ASCII only, no labels, no goto,
rem no parenthesized blocks, no chained - every line here stands alone.
setlocal
set "CHROME=%ProgramFiles%\Google\Chrome\Application\chrome.exe"
if exist "%CHROME%" set "HAVE=1"
if not defined HAVE set "CHROME=%ProgramFiles(x86)%\Google\Chrome\Application\chrome.exe"
if exist "%CHROME%" set "HAVE=1"
if not defined HAVE echo Chrome was not found where this file expected it:
if not defined HAVE echo     %ProgramFiles%\Google\Chrome\Application\chrome.exe
if not defined HAVE echo Edit this file and point CHROME at your chrome.exe, then run it again.
if not defined HAVE pause
if not defined HAVE exit /b 1
"%CHROME%" --no-first-run "--user-data-dir=%~dp0profile" --disable-backgrounding-occluded-windows --disable-renderer-backgrounding --disable-background-timer-throttling --remote-debugging-port=9223
