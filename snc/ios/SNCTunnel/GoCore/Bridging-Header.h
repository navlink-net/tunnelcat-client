// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

// Bridging header for the SNCTunnel Network Extension.
//
// Declares the C entry points exported by the Go static library
// (ios-client/cmd/snc-core/lib_ios.go compiled via build.sh).
//
// After running build.sh, the auto-generated ios-client/build/libsnc_core.h
// can be included here instead of these manual declarations if preferred.
// The manual declarations below match the //export function signatures exactly
// and avoid a dependency on CGO's internal GoString / GoInt typedefs.

#ifndef SNCTunnel_Bridging_Header_h
#define SNCTunnel_Bridging_Header_h

#include <stdint.h>

// SNCStart starts the tunnel goroutines inside the Network Extension process.
// Returns 0 on success, -1 on error (bad key, logging failure, etc.).
// tunFD is the utun file descriptor from NEPacketTunnelProvider.
// wildcatMode: 1 = WildCat REST transport, 0 = normal.
// manual: 1 = genuine user-initiated connect, 0 = on-demand/system-triggered --
// feeds the admin dashboard's connection-stats feature (see
// core.ConnStatsCollector.IncConnect on the Go side).
int32_t SNCStart(const char *key, const char *logDir, const char *dataDir,
                 int32_t tunFD, int32_t wildcatMode, int32_t manual);

// SNCStop signals the tunnel goroutines to shut down.
// manual: 1 = genuine user-initiated disconnect (NEProviderStopReason.userInitiated),
// 0 = any other stop reason (superseded, on-demand off, internal failure cleanup, etc.).
void SNCStop(int32_t manual);

// SNCGetStatus returns a JSON string: {"state":"idle|connecting|connected|error","error":"..."}.
// The caller must free the returned pointer with SNCFreeString.
char *SNCGetStatus(void);

// SNCFreeString frees a string returned by Go (via C.CString).
void SNCFreeString(char *s);

// SNCSetWildcat enables (1) or disables (0) WildCat mode and triggers reconnect.
void SNCSetWildcat(int32_t enabled);

// SNCSetWildcatToken passes a fresh access token to the WildCat credential manager.
// Must be called before WildCat mode starts, and again periodically to keep
// the token alive.
void SNCSetWildcatToken(const char *cToken);

// SNCReconnect signals the tunnel to rebuild the dialer pool (call on network change).
void SNCReconnect(void);

// SNCGetLogUploadPref returns a JSON string
// {"enabled":bool,"admin_disabled":bool,"global_enabled":bool,"effective":bool}
// -- this account's log-upload preference, fetched from the arbiter (see
// docs/LOG_UPLOAD_PRIVACY.md). All fields false when there's no live tunnel
// yet. The caller must free the returned pointer with SNCFreeString.
char *SNCGetLogUploadPref(void);

// SNCSetLogUploadPref sets this account's own log-upload preference
// (enabled: 1 = on, 0 = off) and returns the resulting state in the same
// JSON shape as SNCGetLogUploadPref. The caller must free the returned
// pointer with SNCFreeString.
char *SNCSetLogUploadPref(int32_t enabled);

#endif /* SNCTunnel_Bridging_Header_h */
