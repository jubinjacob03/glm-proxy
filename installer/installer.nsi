; GLM Proxy installer.
;
; Per-user install (no administrator prompt) into %LOCALAPPDATA%\Programs, which
; stays writable so the proxy can create tokens.sqlite and logs next to its exe.
; A custom setup page collects the Z.AI token; it can be changed later from the
; tray icon. The tray is registered to start on every login.

Unicode true
SetCompressor /SOLID lzma

!include "MUI2.nsh"
!include "nsDialogs.nsh"
!include "LogicLib.nsh"

!define APP_NAME    "GLM Proxy"
!define APP_ID      "GLM-Proxy"
!define PUBLISHER   "Jubin"
!define APP_VERSION "1.0.0"
!define TRAY_EXE    "glm-tray.exe"
!define UNINST_KEY  "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APP_ID}"
!define RUN_KEY     "Software\Microsoft\Windows\CurrentVersion\Run"

Name "${APP_NAME}"
OutFile "out\GLM-Proxy-Setup.exe"
InstallDir "$LOCALAPPDATA\Programs\${APP_ID}"
InstallDirRegKey HKCU "Software\${APP_ID}" "InstallDir"
RequestExecutionLevel user
ShowInstDetails show
ShowUninstDetails show

!define MUI_ICON   "..\cmd\glm-tray\icon.ico"
!define MUI_UNICON "..\cmd\glm-tray\icon.ico"

; Version metadata — this is where the "publisher" name surfaces in Windows.
VIProductVersion "1.0.0.0"
VIAddVersionKey "ProductName"     "${APP_NAME}"
VIAddVersionKey "CompanyName"     "${PUBLISHER}"
VIAddVersionKey "LegalCopyright"  "Copyright (c) 2026 ${PUBLISHER}"
VIAddVersionKey "FileDescription" "${APP_NAME} Setup"
VIAddVersionKey "FileVersion"     "${APP_VERSION}"
VIAddVersionKey "ProductVersion"  "${APP_VERSION}"

Var ZaiToken
Var TokenBox

; ── Pages ───────────────────────────────────────────────────────────────────
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "..\LICENSE"
!insertmacro MUI_PAGE_DIRECTORY
Page custom TokenPageCreate TokenPageLeave
!insertmacro MUI_PAGE_INSTFILES

!define MUI_FINISHPAGE_RUN "$INSTDIR\${TRAY_EXE}"
!define MUI_FINISHPAGE_RUN_TEXT "Start ${APP_NAME} now"
!define MUI_FINISHPAGE_TEXT "${APP_NAME} is installed and will start automatically every time you log in.$\r$\n$\r$\nOn first run it downloads a browser component and collects device tokens in the background; the first few requests may fail until that finishes (1-2 minutes)."
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

; ── Custom token page ─────────────────────────────────────────────────────────
Function TokenPageCreate
  !insertmacro MUI_HEADER_TEXT "Z.AI Token" "Connect the proxy to your Z.AI account."
  nsDialogs::Create 1018
  Pop $0
  ${If} $0 == error
    Abort
  ${EndIf}

  ${NSD_CreateLabel} 0 0 100% 48u "Paste your Z.AI token (JWT from chat.z.ai).$\r$\n$\r$\nLeave this blank to run in guest mode (glm-4.7 only). You can set or change the token any time from the tray icon > Update token."
  Pop $0

  ${NSD_CreateText} 0 54u 100% 12u "$ZaiToken"
  Pop $TokenBox

  nsDialogs::Show
FunctionEnd

Function TokenPageLeave
  ${NSD_GetText} $TokenBox $ZaiToken
FunctionEnd

; ── Install ───────────────────────────────────────────────────────────────────
Section "Install"
  ; Stop any running instance so files can be overwritten on upgrade.
  nsExec::Exec 'taskkill /F /IM ${TRAY_EXE}'
  nsExec::Exec 'taskkill /F /IM zai-api.exe'
  nsExec::Exec 'taskkill /F /IM token-collector.exe'

  SetOutPath "$INSTDIR"
  File "staging\zai-api.exe"
  File "staging\token-collector.exe"
  File "staging\${TRAY_EXE}"
  File "..\cmd\glm-tray\icon.ico"
  File "..\LICENSE"

  ; Write .env with the collected token. On an upgrade where the user left the
  ; box blank, keep the existing .env rather than wiping a working token.
  ${If} $ZaiToken != ""
    Call WriteEnv
  ${ElseIf} ${FileExists} "$INSTDIR\.env"
    ; keep existing
  ${Else}
    Call WriteEnv
  ${EndIf}

  ; Start Menu shortcut.
  CreateShortcut "$SMPROGRAMS\${APP_NAME}.lnk" "$INSTDIR\${TRAY_EXE}" "" "$INSTDIR\icon.ico"

  ; Autostart at login (per-user, no admin).
  WriteRegStr HKCU "${RUN_KEY}" "${APP_ID}" '"$INSTDIR\${TRAY_EXE}"'

  ; Remember the install dir and register in Add/Remove Programs (per-user).
  WriteRegStr HKCU "Software\${APP_ID}" "InstallDir" "$INSTDIR"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayName"     "${APP_NAME}"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayVersion"  "${APP_VERSION}"
  WriteRegStr HKCU "${UNINST_KEY}" "Publisher"       "${PUBLISHER}"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayIcon"     "$INSTDIR\icon.ico"
  WriteRegStr HKCU "${UNINST_KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
  WriteRegStr HKCU "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegDWORD HKCU "${UNINST_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINST_KEY}" "NoRepair" 1

  WriteUninstaller "$INSTDIR\uninstall.exe"
SectionEnd

; Writes the full .env. Kept in a function so both branches above reuse it.
Function WriteEnv
  FileOpen $0 "$INSTDIR\.env" w
  FileWrite $0 "# Written by the GLM Proxy installer. Rotate the token from the tray icon.$\r$\n"
  FileWrite $0 "ZAI_TOKEN=$ZaiToken$\r$\n"
  FileWrite $0 "AUTH_TOKEN=Jubin$\r$\n"
  FileWrite $0 "AGENT_MODE=true$\r$\n"
  FileWrite $0 "HOST=0.0.0.0$\r$\n"
  FileWrite $0 "PORT=3007$\r$\n"
  FileWrite $0 "LOG_LEVEL=info$\r$\n"
  FileClose $0
FunctionEnd

; ── Uninstall ─────────────────────────────────────────────────────────────────
Section "Uninstall"
  nsExec::Exec 'taskkill /F /IM ${TRAY_EXE}'
  nsExec::Exec 'taskkill /F /IM zai-api.exe'
  nsExec::Exec 'taskkill /F /IM token-collector.exe'

  DeleteRegValue HKCU "${RUN_KEY}" "${APP_ID}"
  DeleteRegKey HKCU "${UNINST_KEY}"
  DeleteRegKey HKCU "Software\${APP_ID}"

  Delete "$SMPROGRAMS\${APP_NAME}.lnk"

  ; Remove the whole install directory, including tokens.sqlite and logs.
  RMDir /r "$INSTDIR"
SectionEnd
