// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build linux && !android

package linux

// AppStatus is the tunnel state reported to the tray UI.
type AppStatus struct {
	Connected     bool   `json:"connected"`
	Connecting    bool   `json:"connecting"`
	Disconnecting bool   `json:"disconnecting"`
	LoggedIn      bool   `json:"loggedIn"`
	Error         bool   `json:"error"`    // last connect/login attempt failed (see TrayApp.setTrayIcon)
	ErrorMsg      string `json:"errorMsg"` // human-readable reason, e.g. "Connect failed: dial tcp ...: timeout"
	Mode          string `json:"mode"`     // "direct"
}

// AppSettings mirrors the persistent user settings.
type AppSettings struct {
	DoH       bool   `json:"doh"`
	BlockQUIC bool   `json:"blockQUIC"`
	Region    string `json:"region"` // "" = Auto; "RU"/"EU"/"US"/"CN"/"XX"
}
