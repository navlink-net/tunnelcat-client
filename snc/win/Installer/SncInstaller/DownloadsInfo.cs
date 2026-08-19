using System.Net.Http;
using System.Text.Json;

namespace SncInstaller;

internal sealed record WindowsDownloadInfo(bool Available, string Version, string Sha256Hex);

/// <summary>
/// Reads the same public, already-live endpoint the site itself and the in-app updater use --
/// no separate manifest for this installer (per the plan: "у нас уже версии есть").
/// </summary>
internal static class DownloadsInfo
{
    private const string InfoUrl = "https://navlink.net/api/downloads/info";
    public const string ZipUrl = "https://navlink.net/download/zip";

    public static async Task<WindowsDownloadInfo> FetchAsync(HttpClient http, CancellationToken ct)
    {
        Logger.Log($"DownloadsInfo: fetching {InfoUrl}");
        using var resp = await http.GetAsync(InfoUrl, ct);
        resp.EnsureSuccessStatusCode();
        await using var stream = await resp.Content.ReadAsStreamAsync(ct);
        using var doc = await JsonDocument.ParseAsync(stream, cancellationToken: ct);

        var win = doc.RootElement.GetProperty("windows");
        var info = new WindowsDownloadInfo(
            Available: win.TryGetProperty("available", out var av) && av.GetBoolean(),
            Version: win.TryGetProperty("version", out var v) ? v.GetString() ?? "" : "",
            Sha256Hex: win.TryGetProperty("hash", out var h) ? h.GetString() ?? "" : "");
        Logger.Log($"DownloadsInfo: windows available={info.Available} version={info.Version} hash={info.Sha256Hex[..Math.Min(16, info.Sha256Hex.Length)]}...");
        return info;
    }
}
