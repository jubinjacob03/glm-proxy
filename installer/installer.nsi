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
!include "FileFunc.nsh"

!define APP_NAME    "GLM Proxy"
!define APP_ID      "GLM-Proxy"
!define PUBLISHER   "Jubin"
!define APP_VERSION "2.1.2"
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
; Derived from APP_VERSION so the file version cannot drift from the release tag.
VIProductVersion "${APP_VERSION}.0"
VIAddVersionKey "ProductName"     "${APP_NAME}"
VIAddVersionKey "CompanyName"     "${PUBLISHER}"
VIAddVersionKey "LegalCopyright"  "Copyright (c) 2026 ${PUBLISHER}"
VIAddVersionKey "FileDescription" "${APP_NAME} Setup"
VIAddVersionKey "FileVersion"     "${APP_VERSION}"
VIAddVersionKey "ProductVersion"  "${APP_VERSION}"

Var ZaiToken
Var ExistingToken
Var TokenBox
Var RemoveData
Var RemoveDataBox

; Driver and Chromium live outside $INSTDIR so an uninstall can preserve them.
!define DATA_DIR "$LOCALAPPDATA\${APP_ID}"

; ── Pages ───────────────────────────────────────────────────────────────────
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "..\LICENSE"
!insertmacro MUI_PAGE_DIRECTORY
Page custom TokenPageCreate TokenPageLeave
!insertmacro MUI_PAGE_INSTFILES

!define MUI_FINISHPAGE_RUN "$INSTDIR\${TRAY_EXE}"
!define MUI_FINISHPAGE_RUN_TEXT "Start ${APP_NAME} now"
!define MUI_FINISHPAGE_TEXT "${APP_NAME} is installed and will start automatically every time you log in.$\r$\n$\r$\nThe browser component and the first device tokens were set up during this install, so there is nothing left to download on first launch. If either step reported a problem above, the proxy finishes it on its own instead. Right-click the tray icon and choose Monitor Logs to watch it work."
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
UninstPage custom un.DataPageCreate un.DataPageLeave
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

; ── Custom token page ─────────────────────────────────────────────────────────
; Reads ZAI_TOKEN out of an existing .env, so a reinstall over kept data shows the
; token already in place instead of an empty box. $INSTDIR is final by this point:
; the token page comes after the directory page.
Function ReadExistingToken
  StrCpy $ExistingToken ""
  ${IfNot} ${FileExists} "$INSTDIR\.env"
    Return
  ${EndIf}
  ClearErrors
  FileOpen $R0 "$INSTDIR\.env" r
  ${If} ${Errors}
    Return
  ${EndIf}
  ReadLoop:
    ClearErrors
    FileRead $R0 $R1
    ${If} ${Errors}
      Goto ReadDone
    ${EndIf}
    StrCpy $R2 $R1 10
    ${If} $R2 == "ZAI_TOKEN="
      StrCpy $R3 $R1 "" 10
      ; FileRead keeps the line ending, which would otherwise join the token.
      TrimLoop:
        StrCpy $R4 $R3 1 -1
        ${If} $R4 == "$\r"
        ${OrIf} $R4 == "$\n"
          StrCpy $R3 $R3 -1
          Goto TrimLoop
        ${EndIf}
      StrCpy $ExistingToken $R3
      Goto ReadDone
    ${EndIf}
    Goto ReadLoop
  ReadDone:
  FileClose $R0
FunctionEnd

; A copy out of the DevTools table can arrive wrapped in quotes or with a trailing
; newline, either of which would land in .env verbatim and break every request.
Function TrimToken
  TrimAgain:
  TrimLead:
    StrCpy $R0 $ZaiToken 1
    ${If} $R0 == " "
    ${OrIf} $R0 == "$\t"
    ${OrIf} $R0 == "$\r"
    ${OrIf} $R0 == "$\n"
      StrCpy $ZaiToken $ZaiToken "" 1
      Goto TrimLead
    ${EndIf}
  TrimTail:
    StrCpy $R0 $ZaiToken 1 -1
    ${If} $R0 == " "
    ${OrIf} $R0 == "$\t"
    ${OrIf} $R0 == "$\r"
    ${OrIf} $R0 == "$\n"
      StrCpy $ZaiToken $ZaiToken -1
      Goto TrimTail
    ${EndIf}
  StrCpy $R0 $ZaiToken 1
  StrCpy $R1 $ZaiToken 1 -1
  ${If} $R0 == '"'
  ${AndIf} $R1 == '"'
    ; Each pass strips a character, so this cannot spin.
    StrCpy $ZaiToken $ZaiToken -1 1
    Goto TrimAgain
  ${EndIf}
FunctionEnd

Function TokenPageCreate
  !insertmacro MUI_HEADER_TEXT "Z.AI Token (required)" "The proxy cannot run without one. Here is how to find yours."
  Call ReadExistingToken
  ; Only seed the box on first display, so going Back does not discard a new token.
  ${If} $ZaiToken == ""
    StrCpy $ZaiToken $ExistingToken
  ${EndIf}

  nsDialogs::Create 1018
  Pop $0
  ${If} $0 == error
    Abort
  ${EndIf}

  ${If} $ExistingToken != ""
    ${NSD_CreateLabel} 0 0 100% 80u "The token from your previous install is already filled in below. Leave it as it is to carry on using it, or replace it with a new one.$\r$\n$\r$\nTo fetch a fresh token:$\r$\n1. Sign in at https://chat.z.ai$\r$\n2. Press F12 to open developer tools$\r$\n3. Open the Application tab, then Local Storage on the left$\r$\n4. Expand it and click https://chat.z.ai to show the key/value table$\r$\n5. Scroll down to the token row, copy its Value and paste it below"
  ${Else}
    ${NSD_CreateLabel} 0 0 100% 80u "A Z.AI token is required: image generation and the GLM-5.2-Turbo route do not work without one.$\r$\n$\r$\nHow to get it:$\r$\n1. Sign in at https://chat.z.ai$\r$\n2. Press F12 to open developer tools$\r$\n3. Open the Application tab, then Local Storage on the left$\r$\n4. Expand it and click https://chat.z.ai to show the key/value table$\r$\n5. Scroll down to the token row, copy its Value and paste it below"
  ${EndIf}
  Pop $0

  ${NSD_CreateText} 0 84u 100% 12u "$ZaiToken"
  Pop $TokenBox

  ${NSD_CreateLabel} 0 100u 100% 24u "The value is one long line starting with eyJ. You can change it later from the tray icon > Change token."
  Pop $0

  nsDialogs::Show
FunctionEnd

Function TokenPageLeave
  ${NSD_GetText} $TokenBox $ZaiToken
  Call TrimToken

  ${If} $ZaiToken == ""
    MessageBox MB_ICONEXCLAMATION|MB_OK "A Z.AI token is required to continue.$\r$\n$\r$\nFollow the five steps above to copy it from chat.z.ai, then paste it into the box."
    Abort
  ${EndIf}

  ; Shape check only, and skippable: an upstream format change must not lock
  ; anyone out of setup.
  StrCpy $R0 $ZaiToken 3
  ${If} $R0 != "eyJ"
    MessageBox MB_ICONEXCLAMATION|MB_YESNO "That does not look like a Z.AI token. A token is one long line beginning with eyJ.$\r$\n$\r$\nCheck that you copied the Value of the token row rather than its name.$\r$\n$\r$\nUse it anyway?" IDYES TokenAccepted
    Abort
    TokenAccepted:
  ${EndIf}
FunctionEnd

; ── Uninstaller data page ─────────────────────────────────────────────────────
; Unchecked by default: kept data means no ~700 MB re-download and tokens survive.
Function un.DataPageCreate
  !insertmacro MUI_HEADER_TEXT "Application data" "Keep your tokens and browser, or wipe everything."
  nsDialogs::Create 1018
  Pop $0
  ${If} $0 == error
    Abort
  ${EndIf}

  ${NSD_CreateLabel} 0 0 100% 62u "By default your data is kept, so reinstalling later skips the ~700 MB browser download and your collected tokens stay usable.$\r$\n$\r$\nKept: tokens.sqlite, .env (your Z.AI token), and ${DATA_DIR} (the Playwright browser).$\r$\n$\r$\nTick the box below to remove all of it instead. This cannot be undone."
  Pop $0

  ${NSD_CreateCheckbox} 0 68u 100% 10u "Delete all application data (clean wipe)"
  Pop $RemoveDataBox
  ${If} $RemoveData == 1
    ${NSD_Check} $RemoveDataBox
  ${EndIf}

  nsDialogs::Show
FunctionEnd

Function un.DataPageLeave
  ${NSD_GetState} $RemoveDataBox $RemoveData
FunctionEnd

; ── Install ───────────────────────────────────────────────────────────────────
Section "Install"
  ; Stop any running instance so files can be overwritten on upgrade.
  nsExec::Exec 'taskkill /F /IM ${TRAY_EXE} /IM zai-api.exe /IM token-collector.exe'
  Pop $0

  SetOutPath "$INSTDIR"
  File "staging\zai-api.exe"
  File "staging\token-collector.exe"
  File "staging\${TRAY_EXE}"
  File "..\cmd\glm-tray\icon.ico"
  File "..\LICENSE"

  ; Silent installs skip the page, so an unattended upgrade must read the token here.
  ${If} $ExistingToken == ""
    Call ReadExistingToken
  ${EndIf}

  ; Edited in place: WriteEnv emits fixed lines and would reset anything tuned.
  ${If} $ZaiToken == $ExistingToken
  ${AndIf} $ExistingToken != ""
    DetailPrint "Token unchanged; keeping the existing .env as it is."
  ${ElseIf} $ZaiToken != ""
    Call UpdateEnvToken
  ${ElseIf} ${FileExists} "$INSTDIR\.env"
    DetailPrint "No token entered; keeping the existing .env."
  ${Else}
    Call WriteEnv
  ${EndIf}

  ; The tray's updater logs here before the supervision loop even starts.
  CreateDirectory "$INSTDIR\logs"

  ; Start Menu shortcut.
  CreateShortcut "$SMPROGRAMS\${APP_NAME}.lnk" "$INSTDIR\${TRAY_EXE}" "" "$INSTDIR\icon.ico"

  ; Written before the registry entries below: if it fails (AV, disk full after the
  ; 700 MB download), .onInstFailed unwinds the keys so Add/Remove Programs never
  ; shows an entry whose UninstallString points at a file that was never created.
  WriteUninstaller "$INSTDIR\uninstall.exe"

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

  ; First-launch work moved here, so starting the app only starts the proxy. Both
  ; steps are best-effort, and they run after registration on purpose: an aborted
  ; download still leaves an install that Add/Remove Programs can remove.

  ; nsExec pipes stdout into the details log, which shows escape codes literally.
  System::Call 'kernel32::SetEnvironmentVariable(t "NO_COLOR", t "1")'

  ; --deadline, not nsExec /TIMEOUT: the shipped nsExec ignores that flag (measured, a
  ; 14s command under /TIMEOUT=2000 still returned 0), so the child bounds itself.
  DetailPrint "Downloading the browser component (one time, ~700 MB on disk)..."
  nsExec::ExecToLog '"$INSTDIR\token-collector.exe" --install-browsers --deadline 900'
  Pop $0
  ${If} $0 != 0
    DetailPrint "Browser download did not finish ($0). The proxy will fetch it when first needed."
  ${EndIf}

  ; --skip-if-stocked returns at once when a kept database already has tokens.
  DetailPrint "Collecting the first device tokens..."
  nsExec::ExecToLog '"$INSTDIR\token-collector.exe" --no-tui --batch 2 --db-path "$INSTDIR\tokens.sqlite" --skip-if-stocked 50 --deadline 600'
  Pop $0
  ${If} $0 != 0
    DetailPrint "Token collection did not finish ($0). The proxy collects them on demand instead."
  ${EndIf}

  ; Clear it again: the finish page starts the tray as a child of this process, so
  ; leaving NO_COLOR set would disable colour in the app until the next login.
  System::Call 'kernel32::SetEnvironmentVariable(t "NO_COLOR", p 0)'

  ; A silent update run started from the tray passes /RESTART so the freshly
  ; installed tray comes straight back. Without it the app returns at the next
  ; login through the Run key above.
  ${GetParameters} $R0
  ClearErrors
  ${GetOptions} $R0 "/RESTART" $R1
  ${IfNot} ${Errors}
    Exec '"$INSTDIR\${TRAY_EXE}"'
  ${EndIf}
SectionEnd

; Replaces only the ZAI_TOKEN line, so rotating a token cannot reset a tuned PORT or
; LOG_LEVEL. WriteEnv is for a fresh install, where there is nothing to preserve.
Function UpdateEnvToken
  ClearErrors
  FileOpen $R0 "$INSTDIR\.env" r
  ${If} ${Errors}
    Call WriteEnv
    Return
  ${EndIf}
  FileOpen $R1 "$INSTDIR\.env.new" w
  ${If} ${Errors}
    FileClose $R0
    Return
  ${EndIf}
  StrCpy $R5 0   ; whether a ZAI_TOKEN line was seen

  CopyLoop:
    ClearErrors
    FileRead $R0 $R2
    ${If} ${Errors}
      Goto CopyDone
    ${EndIf}
    StrCpy $R3 $R2 10
    ${If} $R3 == "ZAI_TOKEN="
      FileWrite $R1 "ZAI_TOKEN=$ZaiToken$\r$\n"
      StrCpy $R5 1
    ${Else}
      FileWrite $R1 $R2
    ${EndIf}
    Goto CopyLoop
  CopyDone:

  ; A file that never had the key still needs it.
  ${If} $R5 == 0
    FileWrite $R1 "ZAI_TOKEN=$ZaiToken$\r$\n"
  ${EndIf}
  FileClose $R0
  FileClose $R1

  ; If .env is locked (a scanner, an editor, the running proxy) the replace fails.
  ; Better to keep the old token than to orphan .env.new holding the new one in
  ; plaintext while setup reports success.
  ClearErrors
  Delete "$INSTDIR\.env"
  ${If} ${Errors}
    Delete "$INSTDIR\.env.new"
    DetailPrint "Could not replace .env (it is in use); the token was left unchanged."
    Return
  ${EndIf}
  ClearErrors
  Rename "$INSTDIR\.env.new" "$INSTDIR\.env"
  ${If} ${Errors}
    DetailPrint "WARNING: .env was removed but .env.new could not be renamed into place."
  ${EndIf}
FunctionEnd

; A failed install (e.g. WriteUninstaller quarantined by AV) unwinds the ARP and Run
; entries, so the user is never left with an entry that cannot uninstall itself.
Function .onInstFailed
  DeleteRegKey HKCU "${UNINST_KEY}"
  DeleteRegValue HKCU "${RUN_KEY}" "${APP_ID}"
FunctionEnd

; Writes the full .env. Kept in a function so both branches above reuse it.
Function WriteEnv
  ClearErrors
  FileOpen $0 "$INSTDIR\.env" w
  ${If} ${Errors}
    ; Unchecked, the FileWrites below silently no-op and setup would claim success
    ; with no .env at all, leaving the proxy tokenless.
    MessageBox MB_ICONEXCLAMATION|MB_OK "Setup could not write its configuration file:$\r$\n$INSTDIR\.env$\r$\n$\r$\nClose anything using that folder and reinstall, or set the token later from the tray."
    Return
  ${EndIf}
  FileWrite $0 "# Written by the GLM Proxy installer. Rotate the token from the tray icon.$\r$\n"
  FileWrite $0 "ZAI_TOKEN=$ZaiToken$\r$\n"
  FileWrite $0 "AUTH_TOKEN=Jubin,unlimited$\r$\n"
  FileWrite $0 "AGENT_MODE=true$\r$\n"
  FileWrite $0 "HOST=127.0.0.1$\r$\n"
  FileWrite $0 "PORT=3007$\r$\n"
  FileWrite $0 "LOG_LEVEL=info$\r$\n"
  FileClose $0
FunctionEnd

; ── Uninstall ─────────────────────────────────────────────────────────────────
Section "Uninstall"
  nsExec::Exec 'taskkill /F /IM ${TRAY_EXE} /IM zai-api.exe /IM token-collector.exe'
  Pop $0

  DeleteRegValue HKCU "${RUN_KEY}" "${APP_ID}"
  DeleteRegKey HKCU "${UNINST_KEY}"

  Delete "$SMPROGRAMS\${APP_NAME}.lnk"

  ${If} $RemoveData == 1
    DetailPrint "Removing all application data..."
    DeleteRegKey HKCU "Software\${APP_ID}"
    RMDir /r "$INSTDIR"
    RMDir /r "${DATA_DIR}"
  ${Else}
    ; Software\${APP_ID} holds InstallDir and is left behind on purpose: it is what
    ; makes the next install default back here and find the data it kept.
    DetailPrint "Keeping tokens.sqlite, .env and the downloaded browser."
    Delete "$INSTDIR\zai-api.exe"
    Delete "$INSTDIR\token-collector.exe"
    Delete "$INSTDIR\${TRAY_EXE}"
    Delete "$INSTDIR\icon.ico"
    Delete "$INSTDIR\LICENSE"
    Delete "$INSTDIR\uninstall.exe"
    ; A rotation interrupted mid-write can leave this; do not keep a stray token file.
    Delete "$INSTDIR\.env.new"
    RMDir /r "$INSTDIR\logs"
    RMDir /r "$INSTDIR\update-staging"
    ; Non-recursive: succeeds only if nothing was kept, which is the intent.
    RMDir "$INSTDIR"
  ${EndIf}
SectionEnd
