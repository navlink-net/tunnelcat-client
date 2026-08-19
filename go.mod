module shortnerdcat

go 1.26.2

require tunnel_cat v0.0.0

require (
	github.com/getlantern/systray v1.2.2
	github.com/google/uuid v1.6.0
	github.com/jchv/go-webview2 v0.0.0-20260205173254-56598839c808
	github.com/makiuchi-d/gozxing v0.1.1
	github.com/xjasonlyu/tun2socks/v2 v2.6.0
	golang.org/x/image v0.39.0
	golang.org/x/sys v0.43.0
)

require (
	github.com/ajg/form v1.5.1 // indirect
	github.com/andybalholm/brotli v1.1.1 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/getlantern/context v0.0.0-20190109183933-c447772a6520 // indirect
	github.com/getlantern/errors v0.0.0-20190325191628-abdb3e3e36f7 // indirect
	github.com/getlantern/golog v0.0.0-20190830074920-4ef2e798c2d7 // indirect
	github.com/getlantern/hex v0.0.0-20190417191902-c6586a6fe0b7 // indirect
	github.com/getlantern/hidden v0.0.0-20190325191715-f02dbb02be55 // indirect
	github.com/getlantern/ops v0.0.0-20190325191751-d70cb0d6f85f // indirect
	github.com/go-chi/chi/v5 v5.2.5 // indirect
	github.com/go-chi/cors v1.2.1 // indirect
	github.com/go-chi/render v1.0.3 // indirect
	github.com/go-gost/relay v0.5.0 // indirect
	github.com/go-stack/stack v1.8.0 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510 // indirect
	github.com/gorilla/schema v1.4.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/jchv/go-winloader v0.0.0-20250406163304-c1995be93bd1 // indirect
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/oxtoacart/bpool v0.0.0-20190530202638-03653db5a59c // indirect
	github.com/refraction-networking/utls v1.8.2 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.1 // indirect
	golang.org/x/crypto v0.50.0 // indirect
	golang.org/x/net v0.52.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	golang.org/x/xerrors v0.0.0-20220907171357-04be3eba64a2 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	golang.zx2c4.com/wireguard v0.0.0-20250521234502-f333402bd9cb // indirect
	gvisor.dev/gvisor v0.0.0-20250523182742-eede7a881b20 // indirect
)

replace github.com/sagernet/sing-box => github.com/NavLinkNet/sing-box v1.14.0-alpha.13.0.20260818223027-af33df7f1c34

replace tunnel_cat => ../tunnel_cat

// go.mod replace directives are not transitive -- tunnel_cat/go.mod replaces
// this with its local anet-stub (the real package's linkname reference into
// net.zoneCache breaks against newer Go toolchains), but that only applies
// when building inside the tunnel_cat module itself. shortnerdcat imports
// tunnel_cat as a dependency, so it needs its own copy of the same replace
// or it silently falls back to the real (broken) upstream package.
replace github.com/wlynxg/anet => ../tunnel_cat/anet-stub
