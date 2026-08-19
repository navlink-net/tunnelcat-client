// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
	"tunnel_cat/snc/core"
)

// uiDir is the working directory for UI assets written to disk at runtime.
const uiDir = `C:\.shortnerdcat\ui`

// BrowserWindow is a standalone WebView2 window that acts as a full browser.
// A persistent navigation bar (back/forward/refresh + URL input) is injected
// into every page via Init so it floats as a fixed overlay regardless of the
// page being viewed.  The window is created lazily on first Show() and kept
// alive (hidden) for subsequent show/hide cycles.
type BrowserWindow struct {
	startOnce sync.Once
	readyCh   chan struct{}
	wv        webview2.WebView
}

// NewBrowserWindow creates a BrowserWindow.  Call Show() to open it.
func NewBrowserWindow() *BrowserWindow {
	return &BrowserWindow{readyCh: make(chan struct{})}
}

// Show opens (or un-hides) the browser window.  Creates it on first call.
func (bw *BrowserWindow) Show() {
	bw.startOnce.Do(func() { go bw.runLoop() })
	// Wait until WebView2 is ready, then show.
	go func() {
		<-bw.readyCh
		if bw.wv == nil {
			return
		}
		bw.wv.Dispatch(func() {
			hwnd := uintptr(bw.wv.Window())
			winShowWindowProc.Call(hwnd, wvSWRestore)
			winSetForegroundWindow.Call(hwnd)
		})
	}()
}

// Hide hides the browser window without destroying it.
func (bw *BrowserWindow) Hide() {
	select {
	case <-bw.readyCh:
	default:
		return
	}
	if bw.wv != nil {
		bw.wv.Dispatch(func() {
			winShowWindowProc.Call(uintptr(bw.wv.Window()), wvSWHide)
		})
	}
}

// Destroy shuts down the browser window.  Called on app quit.
func (bw *BrowserWindow) Destroy() {
	select {
	case <-bw.readyCh:
		if bw.wv != nil {
			bw.wv.Terminate()
		}
	default:
	}
}

func (bw *BrowserWindow) runLoop() {
	defer func() {
		if r := recover(); r != nil {
			core.Log.Printf("browser: runLoop PANIC: %v\n%s", r, debug.Stack())
			close(bw.readyCh)
		}
	}()
	runtime.LockOSThread()

	core.Log.Printf("browser: creating WebView2...")
	wv := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:    false,
		DataPath: `C:\.shortnerdcat\webview2-browser`,
		WindowOptions: webview2.WindowOptions{
			Title:  "ShortNerdCat Browser",
			Width:  1100,
			Height: 720,
			Center: true,
		},
		AutoFocus: true,
	})
	if wv == nil {
		core.Log.Printf("browser: WebView2 init failed")
		close(bw.readyCh)
		return
	}
	core.Log.Printf("browser: WebView2 ready hwnd=0x%x", uintptr(wv.Window()))
	bw.wv = wv

	hwnd := uintptr(wv.Window())
	setWindowIconFromICO(hwnd, icoIdle)

	// Subclass WndProc: hide on WM_CLOSE instead of destroying.
	var bwOrigProc uintptr
	bwCB := windows.NewCallback(func(h, msg, wp, lp uintptr) uintptr {
		defer func() {
			if r := recover(); r != nil {
				core.Log.Printf("browser: WndProc PANIC: %v\n%s", r, debug.Stack())
			}
		}()
		if msg == wvWMClose {
			winShowWindowProc.Call(h, wvSWHide)
			return 0
		}
		if bwOrigProc == 0 {
			r, _, _ := winDefWindowProcW.Call(h, msg, wp, lp)
			return r
		}
		r, _, _ := winCallWindowProcW.Call(bwOrigProc, h, msg, wp, lp)
		return r
	})
	op, _, _ := winSetWindowLongPtrW.Call(hwnd, wvGWLPWndProc, bwCB)
	bwOrigProc = op
	_ = bwCB // prevent GC

	// Inject the navigation bar into every page.
	wv.Init(navBarScript)

	// Navigate to the Navlink home page.
	wv.Navigate("https://www.navlink.net")

	close(bw.readyCh)
	core.Log.Printf("browser: entering Run()")
	wv.Run()
	wv.Destroy()
}

// writeBrowserStartPage writes a minimal dark HTML start page to uiDir and
// returns a file:// URL pointing to it.  Falls back to "about:blank" on error.
func writeBrowserStartPage() string {
	const html = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><style>
*{margin:0;padding:0;box-sizing:border-box}
html,body{width:100%;height:100%;background:#11141A;color:#F4F1E8;font-family:'Manrope',sans-serif}
body{display:flex;align-items:center;justify-content:center}
.hint{opacity:.35;font-size:15px;letter-spacing:.04em}
</style></head>
<body><div class="hint">Type a URL in the address bar above and press Enter</div></body></html>`
	if err := os.MkdirAll(uiDir, 0755); err != nil {
		return "about:blank"
	}
	p := filepath.Join(uiDir, "browser_start.html")
	if err := os.WriteFile(p, []byte(html), 0644); err != nil {
		return "about:blank"
	}
	return "file:///" + filepath.ToSlash(p)
}

// navBarScript is injected via Init() into every page WebView2 loads.
// It creates a fixed 48px top bar with back/forward/refresh and a URL input.
// Background is visually distinct from any page content.
const navBarScript = `(function(){
  if (window.__navBar) return;
  window.__navBar = true;

  var bar = document.createElement('div');
  bar.id = '__navBar';
  bar.style.cssText = [
    'position:fixed','top:0','left:0','right:0','height:48px','z-index:2147483647',
    'background:#1B2028',
    'border-bottom:2px solid #59C8FF55',
    'display:flex','align-items:center','gap:6px','padding:0 10px',
    'box-shadow:0 2px 12px rgba(0,0,0,.8)',
    'font-family:Manrope,Arial,sans-serif',
  ].join(';');

  function btn(text, title, fn) {
    var b = document.createElement('button');
    b.textContent = text; b.title = title;
    b.style.cssText = [
      'background:#252B34','border:1px solid #59C8FF66','color:#59C8FF',
      'border-radius:5px','padding:5px 12px','cursor:pointer','font-size:14px',
      'line-height:1','white-space:nowrap','flex-shrink:0',
    ].join(';');
    b.onmouseenter = function(){ b.style.background='#2f3742'; };
    b.onmouseleave = function(){ b.style.background='#252B34'; };
    b.onclick = fn;
    return b;
  }

  bar.appendChild(btn('â—€','Back',    function(){ history.back();    }));
  bar.appendChild(btn('â–¶','Forward', function(){ history.forward(); }));
  bar.appendChild(btn('âŸ³','Refresh', function(){ location.reload(); }));

  var inp = document.createElement('input');
  inp.type = 'text';
  inp.value = (location.href === 'about:blank' || location.protocol === 'file:') ? '' : location.href;
  inp.placeholder = 'Enter URL and press Enterâ€¦';
  inp.style.cssText = [
    'flex:1','min-width:0',
    'background:#07090D','border:1px solid #59C8FF55','border-radius:5px',
    'color:#F4F1E8','padding:6px 12px','font-size:13px','outline:none',
    'caret-color:#59C8FF',
  ].join(';');
  inp.onfocus = function(){ inp.style.borderColor='#59C8FF'; };
  inp.onblur  = function(){ inp.style.borderColor='#59C8FF55'; };
  inp.addEventListener('keydown', function(e){
    if (e.key === 'Enter') {
      var url = inp.value.trim();
      if (url && !/^[a-zA-Z][a-zA-Z0-9+\-.]*:/.test(url)) url = 'https://' + url;
      if (url) location.href = url;
    }
    if (e.key === 'Escape') { inp.blur(); }
  });
  bar.appendChild(inp);

  // Sync URL bar on every navigation (hashchange, pushState, and title mutations).
  function syncURL() {
    if (inp !== document.activeElement) {
      inp.value = (location.href === 'about:blank' || location.protocol === 'file:') ? '' : location.href;
    }
  }
  window.addEventListener('popstate', syncURL);
  window.addEventListener('hashchange', syncURL);
  // Intercept pushState / replaceState.
  (function(){
    var orig = history.pushState;
    history.pushState = function(){ orig.apply(this, arguments); syncURL(); };
    orig = history.replaceState;
    history.replaceState = function(){ orig.apply(this, arguments); syncURL(); };
  })();

  // Push body down so the bar doesn't overlap page content.
  function applyMargin() {
    if (document.body) document.body.style.marginTop = '48px';
  }
  if (document.body) {
    applyMargin();
  } else {
    document.addEventListener('DOMContentLoaded', applyMargin);
  }
  // Also sync URL once DOM is ready.
  document.addEventListener('DOMContentLoaded', syncURL);

  // Append to <html> â€” works even before <body> exists.
  document.documentElement.appendChild(bar);
})();`
