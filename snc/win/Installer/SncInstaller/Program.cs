using System.Diagnostics;
using System.IO.Compression;
using System.Net.Http;
using System.Windows;
using SncInstaller.UI;

namespace SncInstaller;

internal static class Program
{
    [STAThread]
    private static int Main()
    {
        using var http = new HttpClient { Timeout = TimeSpan.FromMinutes(10) };

        var app = new Application { ShutdownMode = ShutdownMode.OnExplicitShutdown };

        // The async work must run inside Application.Run(), not be awaited/blocked-on before it --
        // see ImmersiveLionInstaller's Program.cs for why (STA/DispatcherSynchronizationContext
        // ordering; this installer's structure is a direct, simplified port of that one).
        var exitCode = 0;
        app.Startup += async (_, _) =>
        {
            Logger.Log("=== SncInstaller starting ===");
            try
            {
                await RunAsync(http, app);
                Logger.Log("RunAsync completed normally");
            }
            catch (OperationCanceledException)
            {
                Logger.Log("RunAsync cancelled by user");
                exitCode = 2;
            }
            catch (Exception ex)
            {
                Logger.Log($"RunAsync failed: {ex}");
                MessageBox.Show($"Setup failed: {ex.Message}", "ShortNerdCat Setup",
                    MessageBoxButton.OK, MessageBoxImage.Error);
                exitCode = 1;
            }
            app.Shutdown(exitCode);
        };

        return app.Run();
    }

    private static async Task RunAsync(HttpClient http, Application app)
    {
        using var cts = new CancellationTokenSource();
        var window = new ProgressWindow();
        window.CancelRequested += () => cts.Cancel();
        window.Show();
        app.MainWindow = window;

        var ct = cts.Token;

        // Step -1: self-defense against the update-loop incident (2026-08-10) -- if a stale
        // pending-installer marker is causing the client to relaunch this installer over and
        // over, there may be piled-up SncInstaller.exe instances and/or leftover marker files.
        // Must run before anything else, even before window status is meaningful.
        Logger.Log("Step -1: Autostart.StopOtherInstallerInstances / CleanStaleUpdateMarkers");
        await Task.Run(Autostart.StopOtherInstallerInstances, ct);
        await Task.Run(Autostart.CleanStaleUpdateMarkers, ct);

        // Step 0: tear down every pre-installer copy of the app (old manual unzip-and-run
        // installs) -- possibly more than one, each with its own stale autostart entry. Must
        // happen before anything else so a currently-running old copy can't hold a lock on
        // shortnerdcat.exe or wintun.dll and make the extract step below fail.
        window.SetStatus("Removing previous installation…");
        Logger.Log("Step 0: Autostart.RemovePreInstallerCopies");
        await Task.Run(Autostart.RemovePreInstallerCopies, ct);

        window.SetStatus("Checking for the latest version…");
        var info = await DownloadsInfo.FetchAsync(http, ct);
        if (!info.Available || string.IsNullOrEmpty(info.Sha256Hex))
        {
            throw new InvalidOperationException("The Windows client isn't currently available for download. Please try again later.");
        }
        window.SetVersionInfo(info.Version);
        Logger.Log($"Target version: {info.Version}");

        AppPaths.EnsureRootExists();

        window.SetStatus("Downloading ShortNerdCat…");
        var downloader = new Downloader(http);
        await downloader.DownloadAsync(DownloadsInfo.ZipUrl, AppPaths.DownloadZipPath, info.Sha256Hex,
            progress => window.SetDownloadProgress("Downloading ShortNerdCat…", progress), ct);

        window.SetStatus("Installing…");
        Logger.Log("Stopping any running instance before extraction");
        await Task.Run(Autostart.StopRunningClient, ct);
        Logger.Log($"Extracting {AppPaths.DownloadZipPath} -> {AppPaths.Root}");
        ExtractOverwrite(AppPaths.DownloadZipPath, AppPaths.Root);
        File.Delete(AppPaths.DownloadZipPath);

        if (!File.Exists(AppPaths.ClientExe))
        {
            throw new InvalidOperationException($"Install verification failed: {AppPaths.ClientExe} not found after extracting.");
        }

        window.SetStatus("Creating shortcut…");
        await Task.Run(ShortcutManager.CreateDesktopShortcut, ct);

        window.SetStatus("Registering autostart…");
        await Task.Run(Autostart.Register, ct);
        await Task.Run(Autostart.MarkInstalledByInstaller, ct);

        window.SetStatus("Starting ShortNerdCat…");
        Logger.Log($"Launching {AppPaths.ClientExe}");
        Process.Start(new ProcessStartInfo(AppPaths.ClientExe)
        {
            WorkingDirectory = AppPaths.Root,
            UseShellExecute = true,
        });

        // Brief pause so the user sees "Starting ShortNerdCat…" rather than the window vanishing
        // the instant the child process is merely requested to start.
        await Task.Delay(800, CancellationToken.None);
        window.Close();
    }

    /// <summary>
    /// ZipFile.ExtractToDirectory(overwriteFiles: true) refuses if the destination has files the
    /// zip doesn't (fine here -- the zip is just shortnerdcat.exe + wintun.dll) but importantly
    /// DOES overwrite existing same-named files, which is exactly what's needed for a re-install/
    /// update over an existing install this same installer already made.
    /// </summary>
    private static void ExtractOverwrite(string zipPath, string destDir)
    {
        ZipFile.ExtractToDirectory(zipPath, destDir, overwriteFiles: true);
    }
}
