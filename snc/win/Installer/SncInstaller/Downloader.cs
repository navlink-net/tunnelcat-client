using System.Net.Http;
using System.Security.Cryptography;

namespace SncInstaller;

public sealed record DownloadProgress(long BytesDownloaded, long TotalBytes, double BytesPerSecond)
{
    public double FractionComplete => TotalBytes <= 0 ? 0 : Math.Clamp((double)BytesDownloaded / TotalBytes, 0, 1);
}

/// <summary>
/// Downloads a single file with progress reporting, SHA-256 verification, and retry with
/// backoff. Much simpler than vschool_server's ImmersiveLionInstaller.Downloader (that one handles
/// dozens of parallel multi-GB files with Range-resume; ShortNerdCat ships one ~40MB zip, so a
/// from-scratch retry on failure is simple and fast enough -- no resume logic needed).
/// </summary>
internal sealed class Downloader
{
    private const int MaxAttempts = 5;
    private static readonly TimeSpan MaxBackoff = TimeSpan.FromSeconds(15);
    private static readonly TimeSpan StallTimeout = TimeSpan.FromSeconds(30);

    private readonly HttpClient _http;

    public Downloader(HttpClient http) => _http = http;

    public async Task DownloadAsync(string url, string destPath, string expectedSha256Hex,
        Action<DownloadProgress> onProgress, CancellationToken ct)
    {
        for (var attempt = 1; ; attempt++)
        {
            try
            {
                await DownloadOnceAsync(url, destPath, expectedSha256Hex, onProgress, ct);
                return;
            }
            catch (Exception ex) when (!ct.IsCancellationRequested
                && ex is IOException or HttpRequestException or InvalidDataException or UnauthorizedAccessException)
            {
                Logger.Log($"Downloader: attempt {attempt}/{MaxAttempts} failed: {ex.GetType().Name}: {ex.Message}");
                if (attempt >= MaxAttempts)
                {
                    throw;
                }
                var backoff = TimeSpan.FromSeconds(Math.Pow(2, attempt));
                await Task.Delay(backoff > MaxBackoff ? MaxBackoff : backoff, ct);
            }
        }
    }

    private async Task DownloadOnceAsync(string url, string destPath, string expectedSha256Hex,
        Action<DownloadProgress> onProgress, CancellationToken ct)
    {
        Directory.CreateDirectory(Path.GetDirectoryName(destPath)!);
        var tempPath = destPath + ".downloading";

        using var sha256 = SHA256.Create();
        using (var response = await _http.GetAsync(url, HttpCompletionOption.ResponseHeadersRead, ct))
        {
            response.EnsureSuccessStatusCode();
            var totalBytes = response.Content.Headers.ContentLength ?? 0;

            await using var httpStream = await response.Content.ReadAsStreamAsync(ct);
            await using var fileStream = new FileStream(tempPath, FileMode.Create, FileAccess.Write);

            using var readCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
            var buffer = new byte[81920];
            long downloaded = 0;
            var stopwatch = System.Diagnostics.Stopwatch.StartNew();

            while (true)
            {
                int read;
                try
                {
                    readCts.CancelAfter(StallTimeout);
                    read = await httpStream.ReadAsync(buffer, readCts.Token);
                }
                catch (OperationCanceledException) when (!ct.IsCancellationRequested)
                {
                    throw new IOException($"Connection stalled (no data for {StallTimeout.TotalSeconds:F0}s)");
                }
                if (read == 0)
                {
                    break;
                }

                await fileStream.WriteAsync(buffer.AsMemory(0, read), ct);
                sha256.TransformBlock(buffer, 0, read, null, 0);
                downloaded += read;

                var speed = stopwatch.Elapsed.TotalSeconds > 0 ? downloaded / stopwatch.Elapsed.TotalSeconds : 0;
                onProgress(new DownloadProgress(downloaded, totalBytes, speed));
            }
            sha256.TransformFinalBlock([], 0, 0);
        }

        var actualHash = Convert.ToHexStringLower(sha256.Hash!);
        if (!string.Equals(actualHash, expectedSha256Hex, StringComparison.OrdinalIgnoreCase))
        {
            File.Delete(tempPath);
            throw new InvalidDataException($"Hash mismatch: expected {expectedSha256Hex}, got {actualHash}");
        }

        File.Move(tempPath, destPath, overwrite: true);
    }
}
