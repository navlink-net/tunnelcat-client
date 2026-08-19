namespace SncInstaller;

/// <summary>
/// Minimal file logger -- appends timestamped lines to %TEMP%\SncInstaller.log. Every line is
/// flushed immediately so the log is useful even if the process never exits cleanly. Ported from
/// vschool_server's ImmersiveLionInstaller.Logger.
/// </summary>
internal static class Logger
{
    private static readonly object Lock = new();
    private static string? _logPath;

    private static string LogPath
    {
        get
        {
            if (_logPath is null)
            {
                _logPath = Path.Combine(Path.GetTempPath(), "SncInstaller.log");
                // Truncate at the start of each run rather than growing forever -- this is a
                // diagnostic aid for "what just happened," not a historical audit log.
                try
                {
                    File.WriteAllText(_logPath, string.Empty);
                }
                catch
                {
                    // Best-effort -- logging must never be the reason the actual install fails.
                }
            }
            return _logPath;
        }
    }

    public static void Log(string message)
    {
        var line = $"[{DateTime.Now:HH:mm:ss.fff}] {message}";
        lock (Lock)
        {
            try
            {
                File.AppendAllText(LogPath, line + Environment.NewLine);
            }
            catch
            {
                // Logging must never be the reason the actual install fails.
            }
        }
    }
}
