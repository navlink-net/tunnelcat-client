namespace SncInstaller;

/// <summary>
/// Fixed install location -- unlike vschool_server's ImmersiveLion (which lets the user pick a
/// drive for a many-GB game and remembers the choice in the registry), ShortNerdCat is a single
/// small exe + wintun.dll, so there's no reason to ask: always installs to the per-user,
/// no-elevation-required LocalAppData folder.
/// </summary>
internal static class AppPaths
{
    public static string Root { get; } =
        Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "ShortNerdCat");

    public static string ClientExe => Path.Combine(Root, "shortnerdcat.exe");
    public static string DownloadZipPath => Path.Combine(Root, "shortnerdcat-download.zip");

    public static void EnsureRootExists() => Directory.CreateDirectory(Root);
}
