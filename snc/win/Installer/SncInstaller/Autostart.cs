using System.Diagnostics;
using Microsoft.Win32;

namespace SncInstaller;

/// <summary>
/// Registers the freshly-installed exe for autostart, and -- critically -- first tears down every
/// pre-installer copy of the app. Before this installer existed, users just unzipped
/// shortnerdcat.exe wherever they liked and the app self-registered itself into the Run key on
/// first successful launch (see tunnel_cat/shortnerdcat's windows/autostart.go, RegisterAutostart).
/// Since users could have downloaded/unzipped it more than once over time, more than one stale Run
/// entry can exist simultaneously, each pointing at a different old path -- clean up ALL of them,
/// not just the canonically-named one, mirroring cleanStaleAutostartEntries' logic (match by exe
/// base filename, not by Run-value name) but going further: this also stops the process if it's
/// running and deletes the old files, since a plain registry cleanup alone would leave an orphaned,
/// still-autostarting old copy running until next reboot.
/// </summary>
internal static class Autostart
{
    private const string RunKeyPath = @"Software\Microsoft\Windows\CurrentVersion\Run";
    private const string AppName = "ShortNerdCat";
    private const string ClientExeName = "shortnerdcat.exe";

    // Must match snc/win/windows/autostart.go's installerFlagKeyPath/
    // installerFlagValueName exactly -- the client reads this flag to decide
    // whether it should self-update directly (flag present) or hand off to
    // a freshly-downloaded installer again (flag absent, pre-installer
    // install). Without this flag, every future client update would need a
    // fresh installer redeploy too, just to get already-migrated machines to
    // notice -- see the 2026-08-10 incident/discussion this fixes.
    private const string InstallerFlagKeyPath = @"Software\ShortNerdCat";
    private const string InstallerFlagValueName = "InstalledByInstaller";

    /// <summary>
    /// Finds every Run-key entry whose command points at a shortnerdcat.exe (any path, any
    /// value name -- including entries the current install itself hasn't written yet), kills a
    /// matching running process if one is found, deletes the old install directory, and removes
    /// the stale registry entry. Safe to call even when nothing was ever installed before (no-op).
    /// </summary>
    public static void RemovePreInstallerCopies()
    {
        using var key = Registry.CurrentUser.OpenSubKey(RunKeyPath, writable: true);
        if (key is null)
        {
            return;
        }

        var newInstallExe = Path.GetFullPath(AppPaths.ClientExe);
        var staleExePaths = new HashSet<string>(StringComparer.OrdinalIgnoreCase);

        foreach (var valueName in key.GetValueNames())
        {
            var raw = key.GetValue(valueName) as string;
            if (string.IsNullOrWhiteSpace(raw))
            {
                continue;
            }
            var exePath = ExtractExePath(raw);
            if (!string.Equals(Path.GetFileName(exePath), ClientExeName, StringComparison.OrdinalIgnoreCase))
            {
                continue; // unrelated Run entry, leave it alone
            }
            Logger.Log($"Autostart: found stale Run entry '{valueName}' -> {exePath}");
            key.DeleteValue(valueName, throwOnMissingValue: false);

            var fullPath = TryGetFullPath(exePath);
            if (fullPath is not null && !string.Equals(fullPath, newInstallExe, StringComparison.OrdinalIgnoreCase))
            {
                staleExePaths.Add(fullPath);
            }
        }

        foreach (var oldExe in staleExePaths)
        {
            KillProcessesRunningFrom(oldExe);
            TryDeleteOldInstall(oldExe);
        }
    }

    /// <summary>
    /// Stops any OTHER running SncInstaller.exe process (self-excluded by PID). Defends
    /// against the loop where a stale pending-installer marker keeps getting re-launched by
    /// the client on every start -- if that's happening, there may already be one or more
    /// SncInstaller.exe instances stacking up; kill them before this run does its own cleanup
    /// and extraction, so they can't race each other or the file-in-use error resurfaces.
    /// </summary>
    public static void StopOtherInstallerInstances()
    {
        var selfId = Environment.ProcessId;
        foreach (var proc in Process.GetProcessesByName("SncInstaller"))
        {
            if (proc.Id == selfId)
            {
                continue;
            }
            try
            {
                Logger.Log($"StopOtherInstallerInstances: stopping pid={proc.Id}");
                proc.Kill(entireProcessTree: true);
                proc.WaitForExit(5000);
            }
            catch (Exception ex)
            {
                Logger.Log($"StopOtherInstallerInstances: couldn't stop pid={proc.Id}: {ex.Message}");
            }
        }
    }

    /// <summary>
    /// Deletes stale marker/temp files left behind by a previous install attempt --
    /// SncInstaller-pending.exe and shortnerdcat-update.exe/.old are the client's own
    /// (tunnel_cat/snc/core/updater.go) hand-off markers, never something a fresh install
    /// should ship with. Left in place, the client's ApplyPendingUpdate finds the stale
    /// pending-installer marker on its very first launch and hands off to the installer
    /// again -- forever (real incident, 2026-08-10: "installer keeps launching in a loop").
    /// The client itself now moves this marker out before launching (see updater.go), but
    /// this is a second, independent line of defense in case an older, unpatched client is
    /// what triggered this install.
    /// </summary>
    public static void CleanStaleUpdateMarkers()
    {
        foreach (var name in new[] { "SncInstaller-pending.exe", "shortnerdcat-update.exe", "shortnerdcat.exe.old", "shortnerdcat-update.zip", "shortnerdcat-installer-update.zip" })
        {
            var path = Path.Combine(AppPaths.Root, name);
            try
            {
                if (File.Exists(path))
                {
                    File.Delete(path);
                    Logger.Log($"CleanStaleUpdateMarkers: deleted {path}");
                }
            }
            catch (Exception ex)
            {
                Logger.Log($"CleanStaleUpdateMarkers: couldn't delete {path}: {ex.Message}");
            }
        }
    }

    /// <summary>
    /// Stops any currently running shortnerdcat.exe, regardless of path -- including the
    /// canonical install path itself. RemovePreInstallerCopies deliberately leaves a process
    /// running from the canonical path alone (it looks like "the new install", not a stale one),
    /// but that's exactly what's actually running when this installer is used to update an
    /// existing installer-managed install: the old instance is still live at that same path when
    /// extraction tries to overwrite it, and ZipFile.ExtractToDirectory fails with "the process
    /// cannot access the file... because it is being used by another process" (real user report,
    /// 2026-08-10). Must run right before extraction, not merged into
    /// RemovePreInstallerCopies -- that step's whole point is cleaning up OTHER paths.
    /// </summary>
    public static void StopRunningClient()
    {
        foreach (var proc in Process.GetProcessesByName("shortnerdcat"))
        {
            try
            {
                var procPath = proc.MainModule?.FileName;
                Logger.Log($"StopRunningClient: stopping running instance pid={proc.Id} ({procPath ?? "unknown path"})");
                proc.Kill(entireProcessTree: true);
                proc.WaitForExit(5000);
            }
            catch (Exception ex)
            {
                Logger.Log($"StopRunningClient: couldn't stop pid={proc.Id}: {ex.Message}");
            }
        }
    }

    /// <summary>Registers the new install for autostart with the canonical value name.</summary>
    public static void Register()
    {
        using var key = Registry.CurrentUser.CreateSubKey(RunKeyPath);
        key.SetValue(AppName, $"\"{AppPaths.ClientExe}\" --watchdog");
        Logger.Log($"Autostart: registered {AppPaths.ClientExe}");
    }

    /// <summary>
    /// Marks this machine as installer-managed so the client's own updater (see
    /// tunnel_cat/snc/core/updater.go) switches from "download and hand off to a fresh
    /// installer" to a direct in-place self-update from here on -- future client releases
    /// only need the "windows" slot bumped, not this installer too.
    /// </summary>
    public static void MarkInstalledByInstaller()
    {
        using var key = Registry.CurrentUser.CreateSubKey(InstallerFlagKeyPath);
        key.SetValue(InstallerFlagValueName, "1");
        Logger.Log("Autostart: marked InstalledByInstaller flag");
    }

    private static void KillProcessesRunningFrom(string exePath)
    {
        foreach (var proc in Process.GetProcessesByName("shortnerdcat"))
        {
            try
            {
                var procPath = proc.MainModule?.FileName;
                if (procPath is not null && string.Equals(procPath, exePath, StringComparison.OrdinalIgnoreCase))
                {
                    Logger.Log($"Autostart: stopping running old copy pid={proc.Id} ({procPath})");
                    proc.Kill(entireProcessTree: true);
                    proc.WaitForExit(5000);
                }
            }
            catch (Exception ex)
            {
                // Best-effort: a process we can't inspect/kill (permissions, already exited between
                // enumeration and access) shouldn't abort the whole cleanup -- the file delete below
                // will just fail too and get logged there instead.
                Logger.Log($"Autostart: couldn't inspect/kill pid={proc.Id}: {ex.Message}");
            }
        }
    }

    private static void TryDeleteOldInstall(string exePath)
    {
        try
        {
            var dir = Path.GetDirectoryName(exePath);
            File.Delete(exePath);
            Logger.Log($"Autostart: deleted old exe {exePath}");

            // Only remove the containing directory if it looks like it was exclusively ours (just
            // the exe + wintun.dll, nothing else a user might have put there) -- never risk deleting
            // a folder that turns out to be someone's Desktop or Downloads because that's where they
            // happened to unzip the old build.
            if (dir is not null && Directory.Exists(dir))
            {
                var remaining = Directory.GetFileSystemEntries(dir);
                var onlyKnownFiles = remaining.All(p =>
                    string.Equals(Path.GetFileName(p), "wintun.dll", StringComparison.OrdinalIgnoreCase));
                if (onlyKnownFiles)
                {
                    Directory.Delete(dir, recursive: true);
                    Logger.Log($"Autostart: removed old install directory {dir}");
                }
            }
        }
        catch (Exception ex)
        {
            Logger.Log($"Autostart: couldn't delete old install at {exePath}: {ex.Message}");
        }
    }

    /// <summary>Run values look like: `"C:\path\exe.exe" --watchdog` or `C:\path\exe.exe`.</summary>
    private static string ExtractExePath(string value)
    {
        value = value.Trim();
        if (value.StartsWith('"'))
        {
            var end = value.IndexOf('"', 1);
            if (end > 0)
            {
                return value[1..end];
            }
        }
        var spaceIdx = value.IndexOf(' ');
        return spaceIdx >= 0 ? value[..spaceIdx] : value;
    }

    private static string? TryGetFullPath(string path)
    {
        try
        {
            return Path.GetFullPath(path);
        }
        catch
        {
            return null;
        }
    }
}
