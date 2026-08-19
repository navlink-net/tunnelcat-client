// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package main

import (
	"context"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	core "tunnel_cat/snc/core"

	"golang.org/x/sys/windows"
)

// detectDeviceCC returns the best available ISO 3166-1 alpha-2 country code
// for the device's current physical location, trying sources in priority order:
//  1. GPS via Windows Location API (most accurate, shows consent dialog once)
//  2. Windows regional setting (configured by user, instant, no dialog)
//  3. IANA timezone â†’ country (instant, no dialog, updates with system clock)
//
// Returns "" if none of the sources yield a result.
func detectDeviceCC() string {
	if cc := gpsCC(5 * time.Second); cc != "" {
		core.Log.Printf("geo: GPS country=%q", cc)
		return cc
	}
	if cc := systemCC(); cc != "" {
		core.Log.Printf("geo: system-locale country=%q", cc)
		return cc
	}
	if cc := core.TimezoneCC(); cc != "" {
		core.Log.Printf("geo: timezone country=%q", cc)
		return cc
	}
	return ""
}

// systemCC returns the ISO country code from the user's Windows regional
// settings via GetLocaleInfoEx(LOCALE_NAME_USER_DEFAULT, LOCALE_SISO3166CTRYNAME).
// This reflects the configured home region, not physical GPS location.
func systemCC() string {
	const LOCALE_SISO3166CTRYNAME = 0x5A
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	proc := kernel32.NewProc("GetLocaleInfoEx")
	buf := make([]uint16, 4) // ISO codes are 2 chars + null terminator
	r, _, _ := proc.Call(
		0, // LOCALE_NAME_USER_DEFAULT = NULL
		LOCALE_SISO3166CTRYNAME,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r == 0 {
		return ""
	}
	cc := strings.ToUpper(windows.UTF16ToString(buf))
	if len(cc) != 2 {
		return ""
	}
	return cc
}

// gpsCC invokes the Windows Location API via PowerShell (System.Device.Location)
// and converts the resulting coordinates to an ISO country code.
// The system will show the location consent dialog the first time.
// Returns "" on error, permission denial, or timeout.
func gpsCC(timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Use System.Device.Location.GeoCoordinateWatcher (.NET, available on all
	// Windows 10+ client machines). InvariantCulture ensures decimal point is ".".
	const ps = `
$ErrorActionPreference='Stop'
try {
    Add-Type -AssemblyName System.Device
    $w=[System.Device.Location.GeoCoordinateWatcher]::new(
        [System.Device.Location.GeoPositionAccuracy]::Default)
    $w.Start($false)
    $end=[DateTime]::UtcNow.AddSeconds(4)
    while($w.Position.Location.IsUnknown -and [DateTime]::UtcNow -lt $end){
        [System.Threading.Thread]::Sleep(250)
    }
    if(-not $w.Position.Location.IsUnknown){
        $ic=[System.Globalization.CultureInfo]::InvariantCulture
        Write-Output ("$($w.Position.Location.Latitude.ToString('F6',$ic)),$($w.Position.Location.Longitude.ToString('F6',$ic))")
    }
    $w.Stop()
} catch {}
`
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-Command", ps)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return ""
	}
	parts := strings.SplitN(line, ",", 2)
	if len(parts) != 2 {
		return ""
	}
	lat, e1 := strconv.ParseFloat(parts[0], 64)
	lon, e2 := strconv.ParseFloat(parts[1], 64)
	if e1 != nil || e2 != nil {
		return ""
	}
	return latLonToCC(lat, lon)
}

// latLonToCC maps WGS-84 coordinates to an ISO country code using country
// bounding boxes. When multiple boxes contain the point, the smallest-area box
// wins (more specific countries take priority over large ones).
// Returns "" if no box matches.
func latLonToCC(lat, lon float64) string {
	type box struct {
		cc                    string
		latMin, latMax        float64
		lonMin, lonMax        float64
	}
	// Listed from smallest to largest; the algorithm picks smallest matching
	// area automatically, so ordering only affects ties.
	boxes := []box{
		// Europe (small states first)
		{"VA", 41.90, 41.91, 12.44, 12.46},
		{"SM", 43.89, 43.99, 12.40, 12.52},
		{"LI", 47.05, 47.27, 9.47, 9.64},
		{"MC", 43.72, 43.77, 7.38, 7.44},
		{"AD", 42.43, 42.66, 1.41, 1.79},
		{"MT", 35.79, 36.08, 14.18, 14.58},
		{"LU", 49.45, 50.18, 5.73, 6.53},
		{"CY", 34.63, 35.70, 32.27, 34.00},
		{"EE", 57.51, 59.70, 21.77, 28.21},
		{"LV", 55.68, 57.99, 20.97, 28.24},
		{"LT", 53.90, 56.45, 20.94, 26.84},
		{"MD", 45.47, 48.47, 26.62, 30.14},
		{"SI", 45.42, 46.88, 13.38, 16.61},
		{"HR", 42.39, 46.56, 13.49, 19.45},
		{"BA", 42.56, 45.28, 15.74, 19.62},
		{"ME", 41.85, 43.56, 18.45, 20.36},
		{"MK", 40.85, 42.37, 20.46, 23.03},
		{"AL", 39.64, 42.67, 19.27, 21.07},
		{"XK", 41.86, 43.27, 20.01, 21.80}, // Kosovo (ISO 3166-1 not assigned but common usage XK)
		{"RS", 42.24, 46.18, 18.83, 22.99},
		{"BG", 41.24, 44.22, 22.36, 28.61},
		{"SK", 47.73, 49.61, 16.83, 22.57},
		{"HU", 45.74, 48.58, 16.11, 22.90},
		{"AT", 46.37, 49.02, 9.53, 17.16},
		{"CH", 45.83, 47.81, 5.96, 10.49},
		{"BE", 49.50, 51.51, 2.54, 6.40},
		{"NL", 50.75, 53.56, 3.36, 7.23},
		{"DK", 54.56, 57.75, 8.07, 15.19},
		{"IE", 51.45, 55.38, -10.48, -6.01},
		{"PT", 36.96, 42.15, -9.52, -6.19},
		{"GR", 34.80, 41.75, 19.37, 29.65},
		{"CZ", 48.55, 51.06, 12.09, 18.87},
		{"RO", 43.62, 48.27, 20.26, 29.76},
		{"BY", 51.26, 56.17, 23.18, 32.78},
		{"UA", 44.39, 52.38, 22.14, 40.23},
		{"FI", 59.81, 70.09, 20.55, 31.59},
		{"NO", 57.97, 71.19, 4.57, 31.10},
		{"SE", 55.34, 69.06, 11.11, 24.17},
		{"PL", 49.00, 54.84, 14.12, 24.15},
		{"IT", 35.49, 47.09, 6.63, 18.52},
		{"ES", 35.17, 43.79, -9.30, 4.33},
		{"FR", 41.33, 51.12, -5.14, 9.56},
		{"DE", 47.27, 55.06, 5.87, 15.04},
		{"GB", 49.95, 58.70, -8.62, 1.77},
		{"TR", 35.82, 42.11, 25.66, 44.82},

		// Former Soviet / Central Asia
		{"AM", 38.84, 41.30, 43.45, 46.63},
		{"AZ", 38.39, 41.90, 44.77, 50.67},
		{"GE", 41.07, 43.59, 39.99, 46.74},
		{"TJ", 36.67, 41.04, 67.34, 75.15},
		{"TM", 35.13, 42.80, 52.44, 66.69},
		{"UZ", 37.18, 45.59, 55.99, 73.15},
		{"KG", 39.19, 43.25, 69.25, 80.28},
		{"KZ", 40.56, 55.45, 50.27, 87.36},

		// Middle East
		{"QA", 24.47, 26.18, 50.74, 51.62},
		{"BH", 25.79, 26.33, 50.36, 50.82},
		{"KW", 28.53, 30.10, 46.55, 48.43},
		{"OM", 16.65, 26.40, 51.99, 59.84},
		{"AE", 22.63, 26.09, 51.58, 56.39},
		{"IL", 29.49, 33.34, 34.27, 35.90},
		{"JO", 29.19, 33.37, 34.96, 39.30},
		{"LB", 33.09, 34.69, 35.10, 36.62},
		{"SY", 32.31, 37.33, 35.73, 42.38},
		{"IQ", 29.07, 37.38, 38.79, 48.57},
		{"IR", 25.06, 39.78, 44.03, 63.33},
		{"SA", 16.38, 32.15, 34.49, 55.67},
		{"YE", 12.11, 19.00, 41.67, 54.53},

		// South Asia
		{"LK", 5.92, 9.84, 79.64, 81.88},
		{"NP", 26.35, 30.45, 80.05, 88.20},
		{"BT", 26.71, 28.33, 88.75, 92.13},
		{"BD", 20.59, 26.63, 88.01, 92.67},
		{"PK", 23.64, 37.09, 60.88, 77.84},
		{"IN", 8.07, 37.10, 68.17, 97.40},

		// Southeast Asia
		{"SG", 1.15, 1.47, 103.60, 104.00},
		{"BN", 4.01, 5.05, 114.08, 115.36},
		{"TL", -9.50, -8.13, 124.05, 127.34},
		{"PH", 4.64, 21.12, 116.93, 126.60},
		{"TW", 21.89, 25.30, 119.98, 122.00},
		{"HK", 22.15, 22.57, 113.82, 114.41},
		{"MO", 22.10, 22.22, 113.52, 113.60},
		{"KH", 10.41, 14.69, 102.34, 107.63},
		{"LA", 13.92, 22.50, 100.11, 107.64},
		{"VN", 8.56, 23.39, 102.14, 109.47},
		{"TH", 5.61, 20.46, 97.34, 105.64},
		{"MM", 9.78, 28.55, 92.19, 101.17},
		{"MY", 0.85, 7.36, 99.64, 119.28},
		{"ID", -10.92, 5.90, 95.01, 141.02},

		// East Asia
		{"JP", 24.04, 45.56, 122.93, 153.99},
		{"KR", 33.11, 38.63, 124.61, 129.59},
		{"KP", 37.67, 42.99, 124.18, 130.68},
		{"MN", 41.59, 52.14, 87.76, 119.93},
		{"CN", 18.15, 53.56, 73.55, 134.77},

		// Africa
		{"MU", -20.52, -19.98, 57.30, 57.80},
		{"CV", 14.80, 17.21, -25.36, -22.66},
		{"SC", -9.75, -4.29, 55.22, 56.00},
		{"ST", 0.02, 1.70, 6.47, 7.47},
		{"GW", 10.92, 12.68, -16.72, -13.64},
		{"GM", 13.06, 13.83, -16.81, -14.38},
		{"SL", 6.91, 10.00, -13.30, -10.28},
		{"GN", 7.19, 12.68, -15.08, -7.64},
		{"LR", 4.35, 8.55, -11.49, -7.37},
		{"CI", 4.34, 10.74, -8.60, -2.49},
		{"GH", 4.74, 11.17, -3.26, 1.20},
		{"BJ", 6.22, 12.41, 0.77, 3.85},
		{"TG", 5.93, 11.14, -0.15, 1.81},
		{"BF", 9.40, 15.08, -5.52, 2.40},
		{"ML", 10.14, 25.00, -4.24, 4.25},
		{"NE", 11.69, 23.53, 0.16, 15.93},
		{"NG", 4.27, 13.89, 2.69, 14.68},
		{"CM", 1.65, 13.08, 8.49, 16.19},
		{"CF", 2.22, 11.00, 14.42, 27.46},
		{"TD", 7.44, 23.45, 13.47, 24.00},
		{"SD", 9.35, 22.23, 21.83, 38.60},
		{"SS", 3.49, 12.24, 24.14, 35.95},
		{"ET", 3.42, 15.00, 32.99, 47.99},
		{"ER", 12.36, 18.02, 36.43, 43.14},
		{"DJ", 10.95, 12.71, 41.77, 43.42},
		{"SO", 1.65, 11.98, 41.00, 51.41},
		{"KE", -4.68, 5.03, 33.91, 41.90},
		{"UG", -1.48, 4.23, 29.57, 35.00},
		{"RW", -2.84, -1.05, 28.86, 30.90},
		{"BI", -4.47, -2.30, 28.99, 30.85},
		{"TZ", -11.75, -0.99, 29.34, 40.45},
		{"MZ", -26.87, -10.47, 32.68, 40.84},
		{"MW", -17.13, -9.37, 32.68, 35.92},
		{"ZM", -18.08, -8.22, 21.99, 33.71},
		{"ZW", -22.42, -15.61, 25.24, 33.06},
		{"BW", -26.91, -17.78, 19.98, 29.37},
		{"NA", -28.97, -16.96, 11.72, 25.26},
		{"AO", -18.02, -4.38, 11.66, 24.08},
		{"CG", -5.03, 3.70, 11.16, 18.65},
		{"CD", -13.46, 5.38, 12.18, 31.31},
		{"GA", -3.98, 2.32, 8.70, 14.52},
		{"GQ", 0.92, 3.79, 8.44, 11.34},
		{"SN", 12.31, 16.69, -17.55, -11.36},
		{"MR", 14.72, 27.30, -17.07, -4.83},
		{"MA", 27.67, 35.92, -13.17, -0.99},
		{"DZ", 18.97, 37.09, -8.67, 11.99},
		{"TN", 30.24, 37.54, 7.52, 11.58},
		{"LY", 19.50, 33.17, 9.39, 25.15},
		{"EG", 22.00, 31.67, 24.70, 36.90},
		{"ZA", -34.82, -22.13, 16.46, 32.89},
		{"LS", -30.68, -28.57, 27.01, 29.46},
		{"SZ", -27.37, -25.72, 30.79, 32.14},
		{"MG", -25.61, -11.94, 43.24, 50.49},

		// Americas
		{"TT", 10.03, 10.89, -61.93, -60.90},
		{"BB", 13.05, 13.34, -59.65, -59.42},
		{"LC", 13.70, 14.11, -61.07, -60.87},
		{"VC", 12.58, 13.38, -61.46, -61.12},
		{"GD", 11.99, 12.53, -61.79, -61.59},
		{"DM", 15.21, 15.63, -61.48, -61.24},
		{"AG", 16.90, 17.73, -61.91, -61.67},
		{"KN", 17.10, 17.42, -62.87, -62.54},
		{"BZ", 15.89, 18.50, -89.22, -87.78},
		{"SV", 13.15, 14.45, -90.10, -87.69},
		{"HN", 12.98, 16.01, -89.36, -83.15},
		{"NI", 10.73, 15.02, -87.69, -82.97},
		{"CR", 8.02, 11.22, -85.94, -82.56},
		{"PA", 7.20, 9.65, -83.05, -77.16},
		{"CU", 19.83, 23.27, -84.97, -74.13},
		{"JM", 17.70, 18.53, -78.37, -76.20},
		{"HT", 18.02, 20.09, -74.49, -71.64},
		{"DO", 17.47, 19.93, -72.00, -68.32},
		{"GT", 13.74, 17.82, -92.24, -88.22},
		{"MX", 14.53, 32.72, -117.13, -86.71},
		{"PY", -27.59, -19.29, -62.64, -54.26},
		{"UY", -34.95, -30.11, -58.44, -53.10},
		{"BO", -22.90, -9.67, -69.65, -57.51},
		{"EC", -4.99, 1.44, -80.97, -75.19},
		{"PE", -18.35, -0.04, -81.33, -68.67},
		{"CL", -55.91, -17.50, -75.79, -66.42},
		{"AR", -55.06, -21.78, -73.58, -53.64},
		{"VE", 0.72, 12.20, -73.35, -59.81},
		{"CO", -4.23, 12.47, -81.73, -66.87},
		{"GY", 1.19, 8.56, -61.41, -56.98},
		{"SR", 1.84, 6.01, -58.07, -53.98},
		{"BR", -33.75, 5.27, -73.99, -34.79},
		{"CA", 41.68, 83.11, -141.00, -52.62},
		{"US", 24.39, 49.38, -124.85, -66.93}, // contiguous; AK/HI via timezone

		// Oceania
		{"NZ", -46.64, -34.39, 166.43, 178.55},
		{"PG", -10.65, -1.31, 140.84, 155.98},
		{"AU", -43.64, -10.69, 113.34, 153.64},

		// Russia (large; listed last so smaller European countries win)
		{"RU", 41.18, 81.90, 19.64, 180.00},
	}

	best := ""
	bestArea := math.MaxFloat64
	for _, b := range boxes {
		if lat >= b.latMin && lat <= b.latMax && lon >= b.lonMin && lon <= b.lonMax {
			area := (b.latMax - b.latMin) * (b.lonMax - b.lonMin)
			if area < bestArea {
				bestArea = area
				best = b.cc
			}
		}
	}
	return best
}
