; TaskView — Windows installer (Inno Setup 6).
;
; Per-user installation: no administrator rights, no Windows service, no PATH
; change and no shared machine state. The task database lives in
; %USERPROFILE%\.taskview and is never touched by the installer; it survives
; both an upgrade and an uninstall.
;
; The installation itself is coordinated by the application:
;
;   * the application binary is embedded twice. Once as the installed program
;     ({app}\TaskView.exe) and once as a temporary helper that is extracted to
;     the setup's temporary directory (Flags: dontcopy). The helper keeps the
;     name of the program, it only lives in the temporary directory of the
;     setup, and it never opens the task database.
;   * --internal-install-prepare stops the running instance and hands the
;     installation lock to a background worker. It returns 0 when the
;     installation may continue.
;   * --internal-install-commit installs the launch decision, starts the new
;     version, verifies that it answers and rolls a failed upgrade back to the
;     previous version.
;   * --internal-install-abort rolls an installation back that never reached
;     the commit.
;   * --internal-uninstall stops the running instance and removes the logon
;     task that belongs to this installation.
;
; Exit code 20 of the prepare step means "an installation from an older
; version was found". The user is asked to confirm the migration; a silent
; installation never migrates on its own.

#ifndef MyAppVersion
  #define MyAppVersion "2.0.0"
#endif

#ifndef MyAppSource
  #define MyAppSource "..\dist\TaskView.exe"
#endif

#define MyAppName "TaskView"
#define MyAppPublisher "TaskView"
#define MyAppSupportURL "https://github.com/DarlingCY/TaskView"
#define MyAppUpdatesURL "https://github.com/DarlingCY/TaskView/releases"
#define MyAppId "{{B7C4E2A1-5D93-4F86-9A2C-3E1D6F80B4A7}"
#define MyAppExeName "TaskView.exe"
; The helper keeps the program's own file name: ExtractTemporaryFile extracts
; an embedded file under its Source name, so a different DestName would make
; the extraction unreliable. It still lives only in the setup's temporary
; directory.
#define MyHelperName "TaskView.exe"
#define MyTransactionName "taskview-transaction.json"

[Setup]
AppId={#MyAppId}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppVerName={#MyAppName} {#MyAppVersion}
VersionInfoVersion={#MyAppVersion}
VersionInfoCompany={#MyAppPublisher}
VersionInfoDescription={#MyAppName} local task board
AppPublisher={#MyAppPublisher}
AppPublisherURL={#MyAppSupportURL}
AppSupportURL={#MyAppSupportURL}
AppUpdatesURL={#MyAppUpdatesURL}
DefaultDirName={localappdata}\Programs\{#MyAppName}
DefaultGroupName={#MyAppName}
DisableProgramGroupPage=yes
DisableDirPage=yes
UsePreviousAppDir=no
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
OutputDir=..\dist
OutputBaseFilename=TaskView-Setup-{#MyAppVersion}-windows-amd64
Compression=lzma2/ultra64
SolidCompression=yes
WizardStyle=modern
SetupIconFile=..\resources\taskview.ico
UninstallDisplayIcon={app}\{#MyAppExeName}
UninstallDisplayName={#MyAppName}
LicenseFile=..\LICENSE
SetupLogging=yes
CloseApplications=no
RestartApplications=no

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked
Name: "launch"; Description: "Start {#MyAppName} when the installation finishes"; GroupDescription: "After the installation:"; Flags: checkedonce
Name: "autostart"; Description: "Start {#MyAppName} automatically when you log in to Windows"; GroupDescription: "After the installation:"; Flags: unchecked

[Files]
; The installed program. The version of the file is not compared: the
; installer always writes the program that belongs to this release.
Source: {#MyAppSource}; DestDir: "{app}"; Flags: ignoreversion
; The same binary as the installation helper. dontcopy keeps it out of the
; installation directory; it is extracted to the temporary directory of the
; setup and executed from there.
Source: {#MyAppSource}; DestDir: "{tmp}"; Flags: dontcopy

[Icons]
; Start menu entry, always created.
Name: "{group}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; WorkingDir: "{app}"; IconFilename: "{app}\{#MyAppExeName}"
; Desktop shortcut, only when the user asks for it.
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; WorkingDir: "{app}"; IconFilename: "{app}\{#MyAppExeName}"; Tasks: desktopicon

[Code]
const
  LegacyExitCode = 20;

var
  PrepareDone: Boolean;
  CommitStarted: Boolean;
  CommitMessage: String;
  IsUpgrade: Boolean;

function HelperPath: String;
begin
  Result := ExpandConstant('{tmp}\') + '{#MyHelperName}';
end;

function TransactionPath: String;
begin
  Result := ExpandConstant('{tmp}\') + '{#MyTransactionName}';
end;

function AppExePath: String;
begin
  Result := ExpandConstant('{app}\') + '{#MyAppExeName}';
end;

function LogPath: String;
begin
  Result := ExpandConstant('{tmp}\') + 'taskview-install.log';
end;

{ Extracts the installation helper into the temporary directory of the setup.
  The helper is never placed in the installation directory. }
function EnsureHelper: Boolean;
begin
  Result := FileExists(HelperPath);
  if Result then
    Exit;

  ExtractTemporaryFile('{#MyHelperName}');
  Result := FileExists(HelperPath);
end;

{ Runs the helper. The parameters are passed as a separate argument and every
  path inside them is quoted, so no shell is involved and no path can break
  the command line. }
function RunHelper(const Params: String; var ResultCode: Integer): Boolean;
begin
  Result := Exec(HelperPath, Params, ExpandConstant('{tmp}'), SW_HIDE,
                 ewWaitUntilTerminated, ResultCode);
end;

function PrepareParams(const AllowLegacy: Boolean): String;
begin
  Result := '--internal-install-prepare --target "' + AppExePath + '" --transaction "' +
            TransactionPath + '"';
  if AllowLegacy then
    Result := Result + ' --allow-legacy';
end;

{ Runs the prepare step. A legacy installation (exit code 20) is only
  migrated after the user confirmed it; a silent installation never migrates
  on its own. }
function DoPrepare: String;
var
  ResultCode: Integer;
  Answer: Integer;
begin
  Result := '';

  { The installation directory constant is only available once the install
    step runs, unlike in InitializeSetup, so the upgrade decision is taken here
    - before any file has been written. }
  IsUpgrade := FileExists(AppExePath);
  if not EnsureHelper then
  begin
    Result := 'The TaskView installation helper could not be extracted.';
    Exit;
  end;

  { The coordinator refuses to start when a transaction file already exists. }
  DeleteFile(TransactionPath);
  DeleteFile(TransactionPath + '.decision');

  if not RunHelper(PrepareParams(False), ResultCode) then
  begin
    Result := 'The TaskView installation helper could not be started.';
    Exit;
  end;

  if ResultCode = 0 then
  begin
    PrepareDone := True;
    Exit;
  end;

  if ResultCode <> LegacyExitCode then
  begin
    Result := 'TaskView could not prepare the installation (helper exit code ' +
              IntToStr(ResultCode) + '). See ' + LogPath + ' for details.';
    Exit;
  end;

  if WizardSilent then
  begin
    Result := 'An installation of TaskView from an older version was found. ' +
              'Uninstall that version first, then run this installer again.';
    Exit;
  end;

  Answer := MsgBox('An installation of TaskView from an older version was found.' + #13#10 + #13#10 +
                   'Continue with the installation and migrate the older version? ' +
                   'The task database is kept.',
                   mbConfirmation, MB_YESNO);
  if Answer <> IDYES then
  begin
    Result := 'The installation was cancelled because the older TaskView installation ' +
              'has to be migrated first.';
    Exit;
  end;

  DeleteFile(TransactionPath);
  DeleteFile(TransactionPath + '.decision');
  if not RunHelper(PrepareParams(True), ResultCode) then
  begin
    Result := 'The TaskView installation helper could not be started.';
    Exit;
  end;
  if ResultCode <> 0 then
  begin
    Result := 'TaskView could not migrate the older installation (helper exit code ' +
              IntToStr(ResultCode) + '). See ' + LogPath + ' for details.';
    Exit;
  end;
  PrepareDone := True;
end;

{ Hands the launch and startup decisions to the coordinator. The helper starts
  the new version, verifies that it answers and restores the previous version
  when that fails. }
function DoCommit: String;
var
  ResultCode: Integer;
  Params: String;
  StartDecision: String;
  AutostartDecision: String;
begin
  Result := '';
  if not PrepareDone then
    Exit;

  if WizardIsTaskSelected('autostart') then
    AutostartDecision := 'enable'
  else
    AutostartDecision := 'preserve';

  { An upgrade always brings the new version back up: the running instance was
    stopped for the upgrade and the previous version has to be replaced by the
    new one. A first installation follows the choice of the user. }
  if IsUpgrade then
    StartDecision := 'yes'
  else if WizardIsTaskSelected('launch') then
    StartDecision := 'yes'
  else
    StartDecision := 'no';

  Params := '--internal-install-commit --transaction "' + TransactionPath + '" --autostart ' +
            AutostartDecision + ' --start ' + StartDecision;

  if not RunHelper(Params, ResultCode) then
  begin
    { The coordinator is still waiting for a decision: let it roll back. }
    CommitStarted := False;
    CommitMessage := 'TaskView could not be started after the installation. ' +
                     'The previous version was left in place.';
    Result := CommitMessage;
    Exit;
  end;

  { The decision was handed over. The coordinator owns the outcome from here
    on and has already restored the previous version when it failed. The
    installation is not aborted: an aborted installation would roll the files
    back and could remove the program the coordinator has just restored. }
  CommitStarted := True;
  if ResultCode <> 0 then
  begin
    CommitMessage := 'TaskView was installed, but the new version could not be started. ' +
                     'The previous version was restored (helper exit code ' +
                     IntToStr(ResultCode) + '). See ' + LogPath + ' for details.';
    Result := CommitMessage;
  end;
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := '';
  if PrepareDone then
    Exit;
  Result := DoPrepare;
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  Message: String;
begin
  if CurStep = ssInstall then
  begin
    { PrepareToInstall normally runs first, but an unattended setup must not
      install next to a running TaskView either. }
    if not PrepareDone then
    begin
      Message := DoPrepare;
      if Message <> '' then
        RaiseException(Message);
    end;
    Exit;
  end;
  if CurStep = ssPostInstall then
  begin
    Message := DoCommit;
    if Message <> '' then
      MsgBox(Message, mbError, MB_OK);
  end;
end;

procedure DeinitializeSetup();
var
  ResultCode: Integer;
begin
  { The installation never reached the commit: the coordinator restores the
    previous version and releases the locks. After a commit the coordinator
    has already done that, and a second abort would only wait for a worker
    that no longer exists. }
  if PrepareDone and (not CommitStarted) then
    RunHelper('--internal-install-abort --transaction "' + TransactionPath + '"', ResultCode);
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  ResultCode: Integer;
  TempHelper: String;
begin
  if CurUninstallStep <> usUninstall then
    Exit;

  { The uninstall helper runs from a temporary copy of the installed program.
    The uninstaller removes the program itself afterwards. }
  TempHelper := ExpandConstant('{tmp}\') + '{#MyHelperName}';
  DeleteFile(TempHelper);
  if FileExists(AppExePath) then
    CopyFile(AppExePath, TempHelper, True);

  if FileExists(TempHelper) then
  begin
    { The program has to be stopped before the uninstaller removes it: a
      running executable cannot be deleted on Windows. A helper that cannot
      start is reported instead of failing silently later. }
    if not Exec(TempHelper, '--internal-uninstall --target "' + AppExePath + '"',
                ExpandConstant('{tmp}'), SW_HIDE, ewWaitUntilTerminated, ResultCode) then
      MsgBox('TaskView could not be stopped for uninstallation. Close it from the ' +
             'notification area and run the uninstaller again.', mbError, MB_OK);
  end;
end;
