// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin
// +build darwin

// window_cocoa.m — native macOS window for ShortNerdCat.
//
// Creates a standard titled NSWindow (480×500) containing a WKWebView that
// loads the app UI HTML. Custom URL scheme "sncasset://" serves in-memory
// assets (cat images, background). The standard macOS title bar provides
// dragging and the close traffic-light button; windowShouldClose: hides the
// window instead of destroying it.

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <CoreImage/CoreImage.h>
#include "window_cocoa.h"

// Go callbacks — implemented via //export in window_cgo_darwin.go.
extern void go_snc_connect(void);
extern void go_snc_disconnect(void);
extern void go_snc_hide_window(void);
extern void go_snc_set_settings(const char *settingsJSON);
extern void go_snc_submit_key(const char *keyStr);
extern void go_snc_page_ready(void);
extern void goNavlinkURL(const char *urlStr);
extern void go_snc_have_key_answer(int hasKey);
extern void go_snc_credential_login(const char *email, const char *password);
extern void go_snc_credential_login_key_mode(void);
// Native menu bar callbacks (go_snc_menu_*)
extern void go_snc_menu_login(void);
extern void go_snc_menu_logout(void);
extern void go_snc_menu_connect(void);
extern void go_snc_menu_disconnect(void);
extern void go_snc_menu_toggle_doh(void);
extern void go_snc_menu_toggle_quic(void);
extern void go_snc_menu_region(const char *code);
extern void go_snc_menu_about(void);
extern void go_snc_menu_update(void);
extern void go_snc_menu_quit(void);
// Club / recommend callbacks
extern void go_snc_recommend(const char *usernameStr);
extern void go_snc_club_theme_preview(const char *themeStr);

// ── In-memory asset store ─────────────────────────────────────────────────────

static NSMutableDictionary<NSString *, NSData *> *sncAssets;

// ── Asset URL scheme handler ──────────────────────────────────────────────────

@interface SNCAssetHandler : NSObject <WKURLSchemeHandler>
@end

@implementation SNCAssetHandler

- (void)webView:(WKWebView *)webView startURLSchemeTask:(id<WKURLSchemeTask>)task {
    NSURL *url = task.request.URL;
    // URL form: sncasset://<name>  → host = name, path = ""
    // URL form: sncasset://host/<name> → path = "/name"
    NSString *name = url.host ?: @"";
    if (url.path && url.path.length > 1) {
        name = [url.path substringFromIndex:1]; // strip leading /
    }

    NSData *data = sncAssets[name];
    if (!data) {
        NSError *err = [NSError errorWithDomain:NSURLErrorDomain
                                          code:NSURLErrorFileDoesNotExist
                                      userInfo:nil];
        [task didFailWithError:err];
        return;
    }

    NSString *mime = @"application/octet-stream";
    if ([name hasSuffix:@".png"])  mime = @"image/png";
    if ([name hasSuffix:@".jpg"])  mime = @"image/jpeg";
    if ([name hasSuffix:@".html"]) mime = @"text/html";

    NSURLResponse *resp = [[NSURLResponse alloc]
        initWithURL:url
           MIMEType:mime
expectedContentLength:(NSInteger)data.length
   textEncodingName:nil];
    [task didReceiveResponse:resp];
    [task didReceiveData:data];
    [task didFinish];
}

- (void)webView:(WKWebView *)webView stopURLSchemeTask:(id<WKURLSchemeTask>)task {}

@end

// ── Script message handler ────────────────────────────────────────────────────

@interface SNCMsgHandler : NSObject <WKScriptMessageHandler>
@end

@implementation SNCMsgHandler

- (void)userContentController:(WKUserContentController *)ucc
      didReceiveScriptMessage:(WKScriptMessage *)msg {
    NSString *name = msg.name;
    if ([name isEqualToString:@"sncConnect"]) {
        go_snc_connect();
    } else if ([name isEqualToString:@"sncDisconnect"]) {
        go_snc_disconnect();
    } else if ([name isEqualToString:@"sncHideWindow"]) {
        go_snc_hide_window();
    } else if ([name isEqualToString:@"sncSetSettings"]) {
        NSError *err = nil;
        NSData *json = [NSJSONSerialization dataWithJSONObject:msg.body
                                                       options:0
                                                         error:&err];
        if (!err && json) {
            NSString *s = [[NSString alloc] initWithData:json
                                                encoding:NSUTF8StringEncoding];
            go_snc_set_settings([s UTF8String]);
        }
    } else if ([name isEqualToString:@"sncPageReady"]) {
        go_snc_page_ready();
    } else if ([name isEqualToString:@"sncRecommend"]) {
        if ([msg.body isKindOfClass:[NSString class]]) {
            go_snc_recommend((char *)[(NSString *)msg.body UTF8String]);
        }
    } else if ([name isEqualToString:@"sncClubThemePreview"]) {
        if ([msg.body isKindOfClass:[NSString class]]) {
            go_snc_club_theme_preview((char *)[(NSString *)msg.body UTF8String]);
        }
    }
}

@end

// ── Window delegate (main app window) ────────────────────────────────────────

@interface SNCWindowDelegate : NSObject <NSWindowDelegate>
@end

@implementation SNCWindowDelegate

- (BOOL)windowShouldClose:(NSWindow *)sender {
    [sender orderOut:nil];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    return NO;
}

@end

// ── Global state ──────────────────────────────────────────────────────────────

static NSWindow          *sncWin;
static WKWebView         *sncWebView;
static SNCWindowDelegate *sncDelegate;
static SNCMsgHandler     *sncMsgHandler;
static SNCAssetHandler   *sncAssetHandler;
static NSString          *sncHTMLContent;

static const CGFloat kWindowWidth  = 480.0;
static const CGFloat kWindowHeight = 500.0;

// ── C API implementation ──────────────────────────────────────────────────────

void snc_window_register_asset(const char *name, const unsigned char *data, int len) {
    fprintf(stderr, "[snc_window] register_asset: name=%s len=%d thread_is_main=%d\n",
            name, len, (int)[NSThread isMainThread]);
    if (!sncAssets) sncAssets = [NSMutableDictionary dictionary];
    NSString *key = [NSString stringWithUTF8String:name];
    sncAssets[key] = [NSData dataWithBytes:data length:(NSUInteger)len];
    fprintf(stderr, "[snc_window] register_asset: done name=%s\n", name);
}

void snc_window_set_html(const char *html) {
    fprintf(stderr, "[snc_window] set_html: len=%d thread_is_main=%d\n",
            html ? (int)strlen(html) : -1, (int)[NSThread isMainThread]);
    sncHTMLContent = [NSString stringWithUTF8String:html];
    fprintf(stderr, "[snc_window] set_html: done\n");
}

void snc_window_init(void) {
    fprintf(stderr, "[snc_window] init: scheduling on main queue thread_is_main=%d\n",
            (int)[NSThread isMainThread]);
    dispatch_async(dispatch_get_main_queue(), ^{
        @autoreleasepool {
            fprintf(stderr, "[snc_window] init: on main thread, creating NSWindow\n");
            // ── Window ────────────────────────────────────────────────────────
            NSRect frame = NSMakeRect(0, 0, kWindowWidth, kWindowHeight);
            NSWindowStyleMask style = NSWindowStyleMaskTitled
                                    | NSWindowStyleMaskClosable
                                    | NSWindowStyleMaskMiniaturizable;
            sncWin = [[NSWindow alloc]
                initWithContentRect:frame
                          styleMask:style
                            backing:NSBackingStoreBuffered
                              defer:NO];
            fprintf(stderr, "[snc_window] init: NSWindow alloc'd sncWin=%p\n", (void*)sncWin);

            [sncWin setTitle:@"ShortNerdCat"];
            [sncWin setCollectionBehavior:NSWindowCollectionBehaviorMoveToActiveSpace];
            [sncWin setMinSize:NSMakeSize(kWindowWidth, kWindowHeight)];
            [sncWin setMaxSize:NSMakeSize(kWindowWidth, kWindowHeight)];
            [sncWin center];

            sncDelegate = [[SNCWindowDelegate alloc] init];
            [sncWin setDelegate:sncDelegate];
            fprintf(stderr, "[snc_window] init: window configured, creating WKWebView\n");

            // ── WKWebView ─────────────────────────────────────────────────────
            sncAssetHandler = [[SNCAssetHandler alloc] init];
            sncMsgHandler   = [[SNCMsgHandler alloc]   init];
            fprintf(stderr, "[snc_window] init: handlers alloc'd\n");

            WKUserContentController *ucc = [[WKUserContentController alloc] init];
            for (NSString *n in @[@"sncConnect", @"sncDisconnect",
                                  @"sncHideWindow", @"sncSetSettings",
                                  @"sncPageReady", @"sncRecommend",
                                  @"sncClubThemePreview"]) {
                [ucc addScriptMessageHandler:sncMsgHandler name:n];
            }
            fprintf(stderr, "[snc_window] init: script handlers registered\n");

            WKWebViewConfiguration *cfg = [[WKWebViewConfiguration alloc] init];
            [cfg setURLSchemeHandler:sncAssetHandler forURLScheme:@"sncasset"];
            cfg.userContentController = ucc;
            fprintf(stderr, "[snc_window] init: WKWebViewConfiguration done\n");

            sncWebView = [[WKWebView alloc] initWithFrame:frame configuration:cfg];
            fprintf(stderr, "[snc_window] init: WKWebView alloc'd sncWebView=%p\n", (void*)sncWebView);
            [sncWin setContentView:sncWebView];

            // ── Load HTML ─────────────────────────────────────────────────────
            if (sncHTMLContent) {
                fprintf(stderr, "[snc_window] init: loading HTML content\n");
                [sncWebView loadHTMLString:sncHTMLContent
                                   baseURL:[NSURL URLWithString:@"sncasset://host/"]];
                fprintf(stderr, "[snc_window] init: HTML loaded\n");
            } else {
                fprintf(stderr, "[snc_window] init: WARNING sncHTMLContent is nil\n");
            }
            fprintf(stderr, "[snc_window] init: main-thread block complete\n");
        }
    });
    fprintf(stderr, "[snc_window] init: dispatch_async returned (non-blocking)\n");
}

void snc_window_show(void) {
    fprintf(stderr, "[snc_window] show: scheduling on main queue thread_is_main=%d sncWin=%p\n",
            (int)[NSThread isMainThread], (void*)sncWin);
    dispatch_async(dispatch_get_main_queue(), ^{
        @autoreleasepool {
            fprintf(stderr, "[snc_window] show: on main thread sncWin=%p\n", (void*)sncWin);
            if (!sncWin) { fprintf(stderr, "[snc_window] show: sncWin is nil, aborting\n"); return; }
            // Accessory apps can't bring windows to the front without switching
            // to Regular policy first.
            [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
            if (![sncWin isVisible]) [sncWin center];
            [sncWin makeKeyAndOrderFront:nil];
            [NSApp activateIgnoringOtherApps:YES];
        }
    });
}

void snc_window_hide(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        @autoreleasepool {
            if (sncWin) [sncWin orderOut:nil];
            // Return to background-app policy so we don't linger in the Dock.
            [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        }
    });
}

void snc_window_destroy(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        @autoreleasepool {
            [sncWin close];
            sncWin    = nil;
            sncWebView = nil;
        }
    });
}

// evalJS dispatches a JavaScript evaluation to the WebView on the main thread.
// The block captures a copy of the string so the caller's C string can be freed.
static void evalJS(NSString *script) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (sncWebView) [sncWebView evaluateJavaScript:script completionHandler:nil];
    });
}

void snc_window_push_status(const char *statusJSON) {
    NSString *js = [NSString stringWithFormat:
        @"window.onStatusUpdate && window.onStatusUpdate(%s)", statusJSON];
    evalJS(js);
}

void snc_window_push_settings(const char *settingsJSON) {
    NSString *js = [NSString stringWithFormat:
        @"window.onSettingsUpdate && window.onSettingsUpdate(%s)", settingsJSON];
    evalJS(js);
}

void snc_window_push_club_theme(const char *themeJSON) {
    NSString *js = [NSString stringWithFormat:
        @"window.onClubThemeUpdate && window.onClubThemeUpdate(%s)", themeJSON];
    evalJS(js);
}

// ── Native key-entry panel (non-modal) ───────────────────────────────────────
//
// Using a non-modal NSWindow instead of [NSAlert runModal] keeps the main run
// loop free so that the status-bar Quit item remains responsive while the user
// is entering their key.  NSTextField handles Cmd+V paste directly via the
// responder chain — no custom Edit menu is needed.

static BOOL        sncKeyEntryPending = NO;
static NSWindow    *sncKeyPanel       = nil;
static NSTextView  *sncKeyTextView    = nil;

@interface SNCKeyPanelDelegate : NSObject <NSWindowDelegate>
- (void)submitKey:(NSString *)key;
- (void)connectClicked:(id)sender;
- (void)cancelClicked:(id)sender;
@end

@implementation SNCKeyPanelDelegate

- (void)submitKey:(NSString *)key {
    if (!sncKeyEntryPending) return;
    sncKeyEntryPending = NO;
    [sncKeyPanel orderOut:nil];
    sncKeyPanel    = nil;
    sncKeyTextView = nil;
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    go_snc_submit_key([key UTF8String]);
}

- (void)connectClicked:(id)sender {
    NSString *raw = sncKeyTextView ? [sncKeyTextView string] : @"";
    NSString *key = [raw stringByTrimmingCharactersInSet:
                     [NSCharacterSet whitespaceAndNewlineCharacterSet]];
    [self submitKey:key];
}

- (void)cancelClicked:(id)sender { [self submitKey:@""]; }

- (void)scanImageClicked:(id)sender {
    NSOpenPanel *panel = [NSOpenPanel openPanel];
    panel.title                    = @"Select QR Code Image";
    panel.allowsMultipleSelection  = NO;
    panel.canChooseDirectories     = NO;
    panel.allowedFileTypes         = @[@"png", @"jpg", @"jpeg", @"bmp", @"gif", @"tiff", @"heic"];
    [panel beginSheetModalForWindow:sncKeyPanel completionHandler:^(NSModalResponse result) {
        if (result != NSModalResponseOK) return;
        NSURL *url = panel.URLs.firstObject;
        if (!url) return;
        CIImage *ciImg = [CIImage imageWithContentsOfURL:url];
        if (!ciImg) {
            dispatch_async(dispatch_get_main_queue(), ^{
                NSAlert *a = [[NSAlert alloc] init];
                a.messageText    = @"Cannot read image";
                a.informativeText = @"The selected file could not be loaded as an image.";
                [a runModal];
            });
            return;
        }
        NSDictionary *opts = @{CIDetectorAccuracy: CIDetectorAccuracyHigh};
        CIDetector *det = [CIDetector detectorOfType:CIDetectorTypeQRCode
                                              context:nil
                                              options:opts];
        NSArray *features = [det featuresInImage:ciImg];
        NSString *found = nil;
        for (CIFeature *f in features) {
            if ([f isKindOfClass:[CIQRCodeFeature class]]) {
                NSString *msg = ((CIQRCodeFeature *)f).messageString;
                if (msg.length > 0) { found = msg; break; }
            }
        }
        dispatch_async(dispatch_get_main_queue(), ^{
            if (found && sncKeyTextView) {
                [sncKeyTextView setString:found];
            } else {
                NSAlert *a = [[NSAlert alloc] init];
                a.messageText     = @"No QR code found";
                a.informativeText = @"The selected image does not contain a recognizable QR code.";
                [a runModal];
            }
        });
    }];
}

- (void)pasteClicked:(id)sender {
    // The app runs as root (elevated via osascript).  Root has no access to the
    // user's NSPasteboard.  Use launchctl asuser <uid> pbpaste to read the
    // clipboard from the logged-in user's bootstrap context.
    NSString *s = nil;

    // Find the console user's UID via stat /dev/console.
    NSTask *st = [[NSTask alloc] init];
    st.launchPath = @"/usr/bin/stat";
    st.arguments  = @[@"-f", @"%u", @"/dev/console"];
    NSPipe *sp = [NSPipe pipe];
    st.standardOutput = sp;
    st.standardError  = [NSPipe pipe];
    if ([st launchAndReturnError:nil]) {
        [st waitUntilExit];
        NSData   *d   = [[sp fileHandleForReading] readDataToEndOfFile];
        NSString *uid = [[NSString alloc] initWithData:d encoding:NSUTF8StringEncoding];
        uid = [uid stringByTrimmingCharactersInSet:
               [NSCharacterSet whitespaceAndNewlineCharacterSet]];

        NSTask *pb = [[NSTask alloc] init];
        pb.launchPath = @"/bin/launchctl";
        pb.arguments  = @[@"asuser", uid, @"/usr/bin/pbpaste"];
        NSPipe *pp = [NSPipe pipe];
        pb.standardOutput = pp;
        pb.standardError  = [NSPipe pipe];
        if ([pb launchAndReturnError:nil]) {
            [pb waitUntilExit];
            NSData *data = [[pp fileHandleForReading] readDataToEndOfFile];
            s = [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding];
        }
    }

    if (s.length > 0 && sncKeyTextView) {
        [sncKeyTextView setString:s];
    }
}

// Close button → cancel (orderOut is handled in submitKey).
- (BOOL)windowShouldClose:(NSWindow *)sender {
    [self submitKey:@""];
    return NO;
}

@end

static SNCKeyPanelDelegate *sncKeyPanelDelegate = nil;

void snc_window_show_key_entry(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        @autoreleasepool {
            if (sncKeyEntryPending) return;
            sncKeyEntryPending = YES;

            // Become a regular app so the window gets keyboard focus and
            // Cmd+V paste reaches the text view via the responder chain.
            [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
            [NSApp activateIgnoringOtherApps:YES];

            NSRect frame = NSMakeRect(0, 0, 480, 240);
            sncKeyPanel = [[NSWindow alloc]
                initWithContentRect:frame
                          styleMask:NSWindowStyleMaskTitled |
                                    NSWindowStyleMaskClosable
                            backing:NSBackingStoreBuffered
                              defer:NO];
            [sncKeyPanel setTitle:@"ShortNerdCat — Subscription Key"];
            [sncKeyPanel setLevel:NSFloatingWindowLevel];
            [sncKeyPanel center];

            sncKeyPanelDelegate = [[SNCKeyPanelDelegate alloc] init];
            [sncKeyPanel setDelegate:sncKeyPanelDelegate];

            NSView *cv = [sncKeyPanel contentView];

            NSTextField *label = [NSTextField labelWithString:
                @"Paste your ShortNerdCat subscription key:"];
            label.frame = NSMakeRect(20, 192, 440, 20);
            [cv addSubview:label];

            // Multi-line scrollable text view — NSTextView handles Cmd+V natively.
            NSScrollView *sv = [[NSScrollView alloc]
                initWithFrame:NSMakeRect(20, 68, 440, 114)];
            sv.hasVerticalScroller = YES;
            sv.autohidesScrollers  = YES;
            sv.borderType          = NSBezelBorder;

            sncKeyTextView = [[NSTextView alloc] initWithFrame:sv.bounds];
            NSFont *mono = [NSFont fontWithName:@"Menlo" size:11]
                        ?: [NSFont userFixedPitchFontOfSize:11];
            sncKeyTextView.font                             = mono;
            sncKeyTextView.automaticQuoteSubstitutionEnabled  = NO;
            sncKeyTextView.automaticDashSubstitutionEnabled   = NO;
            sncKeyTextView.automaticSpellingCorrectionEnabled = NO;
            sncKeyTextView.continuousSpellCheckingEnabled     = NO;
            [sv setDocumentView:sncKeyTextView];
            [cv addSubview:sv];

            NSButton *pasteBtn = [NSButton buttonWithTitle:@"Paste from Clipboard"
                                                    target:sncKeyPanelDelegate
                                                    action:@selector(pasteClicked:)];
            pasteBtn.frame = NSMakeRect(20, 20, 160, 32);
            [cv addSubview:pasteBtn];

            NSButton *scanImgBtn = [NSButton buttonWithTitle:@"Scan from Image"
                                                      target:sncKeyPanelDelegate
                                                      action:@selector(scanImageClicked:)];
            scanImgBtn.frame = NSMakeRect(188, 20, 87, 32);
            [cv addSubview:scanImgBtn];

            NSButton *cancelBtn = [NSButton buttonWithTitle:@"Cancel"
                                                     target:sncKeyPanelDelegate
                                                     action:@selector(cancelClicked:)];
            cancelBtn.frame = NSMakeRect(283, 20, 90, 32);
            [cv addSubview:cancelBtn];

            NSButton *connectBtn = [NSButton buttonWithTitle:@"Connect"
                                                      target:sncKeyPanelDelegate
                                                      action:@selector(connectClicked:)];
            connectBtn.frame = NSMakeRect(381, 20, 80, 32);
            connectBtn.keyEquivalent = @"\r";
            [cv addSubview:connectBtn];

            [sncKeyPanel makeKeyAndOrderFront:nil];
            [sncKeyPanel makeFirstResponder:sncKeyTextView];
        }
    });
}

void snc_window_cancel_key_entry(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (sncKeyEntryPending && sncKeyPanelDelegate != nil) {
            [sncKeyPanelDelegate submitKey:@""];
        }
    });
}

// ── "Do you have a key?" panel (non-modal) ───────────────────────────────────

static BOOL     sncHaveKeyPending = NO;
static NSWindow *sncHaveKeyPanel  = nil;

@interface SNCHaveKeyDelegate : NSObject <NSWindowDelegate>
- (void)yesClicked:(id)sender;
- (void)noClicked:(id)sender;
@end

@implementation SNCHaveKeyDelegate

- (void)answer:(BOOL)hasKey {
    if (!sncHaveKeyPending) return;
    sncHaveKeyPending = NO;
    [sncHaveKeyPanel orderOut:nil];
    sncHaveKeyPanel = nil;
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    go_snc_have_key_answer(hasKey ? 1 : 0);
}

- (void)yesClicked:(id)sender { [self answer:YES]; }
- (void)noClicked:(id)sender  { [self answer:NO]; }

// Closing the window is treated the same as "No" — the caller still needs an
// answer to proceed, and "No" leads to the more permissive (still safe)
// credential-login-or-key-entry branch rather than leaving the app stuck.
- (BOOL)windowShouldClose:(NSWindow *)sender {
    [self answer:NO];
    return NO;
}

@end

static SNCHaveKeyDelegate *sncHaveKeyDelegate = nil;

void snc_window_show_have_key_prompt(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        @autoreleasepool {
            if (sncHaveKeyPending) return;
            sncHaveKeyPending = YES;

            [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
            [NSApp activateIgnoringOtherApps:YES];

            NSRect frame = NSMakeRect(0, 0, 440, 130);
            sncHaveKeyPanel = [[NSWindow alloc]
                initWithContentRect:frame
                          styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable
                            backing:NSBackingStoreBuffered
                              defer:NO];
            [sncHaveKeyPanel setTitle:@"ShortNerdCat — Get Started"];
            [sncHaveKeyPanel setLevel:NSFloatingWindowLevel];
            [sncHaveKeyPanel center];

            sncHaveKeyDelegate = [[SNCHaveKeyDelegate alloc] init];
            [sncHaveKeyPanel setDelegate:sncHaveKeyDelegate];

            NSView *cv = [sncHaveKeyPanel contentView];

            NSTextField *label = [NSTextField wrappingLabelWithString:
                @"Do you have a ShortNerdCat activation key?"];
            label.frame = NSMakeRect(20, 62, 400, 44);
            [cv addSubview:label];

            NSButton *yesBtn = [NSButton buttonWithTitle:@"Yes, I have a key"
                                                   target:sncHaveKeyDelegate
                                                   action:@selector(yesClicked:)];
            yesBtn.frame = NSMakeRect(20, 20, 190, 32);
            yesBtn.keyEquivalent = @"\r";
            [cv addSubview:yesBtn];

            NSButton *noBtn = [NSButton buttonWithTitle:@"No, I don't have one"
                                                  target:sncHaveKeyDelegate
                                                  action:@selector(noClicked:)];
            noBtn.frame = NSMakeRect(220, 20, 200, 32);
            [cv addSubview:noBtn];

            [sncHaveKeyPanel makeKeyAndOrderFront:nil];
        }
    });
}

// ── Credential (email/password) login panel (non-modal) ─────────────────────

static BOOL           sncLoginPending  = NO;
static NSWindow        *sncLoginPanel   = nil;
static NSTextField     *sncEmailField   = nil;
static NSSecureTextField *sncPasswordField      = nil;
// Plain-text twin of sncPasswordField, same frame, shown instead of it while
// the eye toggle is in "Show" state -- NSSecureTextField has no public API to
// unmask itself, so visibility is achieved by swapping between two fields
// and keeping their string values in sync at toggle time.
static NSTextField     *sncPasswordPlainField = nil;
static NSButton        *sncPasswordEyeBtn     = nil;
static BOOL             sncPasswordShown      = NO;

@interface SNCLoginDelegate : NSObject <NSWindowDelegate>
- (void)loginClicked:(id)sender;
- (void)keyModeClicked:(id)sender;
- (void)cancelClicked:(id)sender;
- (void)eyeClicked:(id)sender;
@end

@implementation SNCLoginDelegate

- (void)dismiss {
    sncLoginPending = NO;
    [sncLoginPanel orderOut:nil];
    sncLoginPanel = nil;
    sncEmailField = nil;
    sncPasswordField = nil;
    sncPasswordPlainField = nil;
    sncPasswordEyeBtn = nil;
    sncPasswordShown = NO;
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
}

- (NSString *)currentPassword {
    return sncPasswordShown
        ? (sncPasswordPlainField ? [sncPasswordPlainField stringValue] : @"")
        : (sncPasswordField ? [sncPasswordField stringValue] : @"");
}

- (void)eyeClicked:(id)sender {
    if (!sncLoginPending) return;
    NSString *current = [self currentPassword];
    sncPasswordShown = !sncPasswordShown;
    if (sncPasswordShown) {
        [sncPasswordPlainField setStringValue:current];
        sncPasswordField.hidden = YES;
        sncPasswordPlainField.hidden = NO;
        [sncPasswordEyeBtn setTitle:@"Hide"];
        [sncLoginPanel makeFirstResponder:sncPasswordPlainField];
    } else {
        [sncPasswordField setStringValue:current];
        sncPasswordPlainField.hidden = YES;
        sncPasswordField.hidden = NO;
        [sncPasswordEyeBtn setTitle:@"Show"];
        [sncLoginPanel makeFirstResponder:sncPasswordField];
    }
}

- (void)loginClicked:(id)sender {
    if (!sncLoginPending) return;
    NSString *email = sncEmailField ? [sncEmailField stringValue] : @"";
    NSString *password = [self currentPassword];
    if (email.length == 0 || password.length == 0) return; // require both, keep panel open
    [self dismiss];
    go_snc_credential_login([email UTF8String], [password UTF8String]);
}

- (void)keyModeClicked:(id)sender {
    if (!sncLoginPending) return;
    [self dismiss];
    go_snc_credential_login_key_mode();
}

- (void)cancelClicked:(id)sender {
    if (!sncLoginPending) return;
    [self dismiss];
    go_snc_credential_login("", "");
}

- (BOOL)windowShouldClose:(NSWindow *)sender {
    [self cancelClicked:sender];
    return NO;
}

@end

static SNCLoginDelegate *sncLoginDelegate = nil;

void snc_window_show_credential_login(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        @autoreleasepool {
            if (sncLoginPending) return;
            sncLoginPending = YES;

            [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
            [NSApp activateIgnoringOtherApps:YES];

            NSRect frame = NSMakeRect(0, 0, 440, 220);
            sncLoginPanel = [[NSWindow alloc]
                initWithContentRect:frame
                          styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable
                            backing:NSBackingStoreBuffered
                              defer:NO];
            [sncLoginPanel setTitle:@"ShortNerdCat — Log In"];
            [sncLoginPanel setLevel:NSFloatingWindowLevel];
            [sncLoginPanel center];

            sncLoginDelegate = [[SNCLoginDelegate alloc] init];
            [sncLoginPanel setDelegate:sncLoginDelegate];

            NSView *cv = [sncLoginPanel contentView];

            NSTextField *emailLabel = [NSTextField labelWithString:@"Email:"];
            emailLabel.frame = NSMakeRect(20, 172, 400, 18);
            [cv addSubview:emailLabel];

            sncEmailField = [[NSTextField alloc] initWithFrame:NSMakeRect(20, 148, 400, 24)];
            [cv addSubview:sncEmailField];

            NSTextField *passLabel = [NSTextField labelWithString:@"Password:"];
            passLabel.frame = NSMakeRect(20, 116, 400, 18);
            [cv addSubview:passLabel];

            sncPasswordField = [[NSSecureTextField alloc] initWithFrame:NSMakeRect(20, 92, 350, 24)];
            [cv addSubview:sncPasswordField];

            sncPasswordPlainField = [[NSTextField alloc] initWithFrame:NSMakeRect(20, 92, 350, 24)];
            sncPasswordPlainField.hidden = YES;
            [cv addSubview:sncPasswordPlainField];

            sncPasswordEyeBtn = [NSButton buttonWithTitle:@"Show"
                                                     target:sncLoginDelegate
                                                     action:@selector(eyeClicked:)];
            sncPasswordEyeBtn.frame = NSMakeRect(378, 90, 42, 24);
            sncPasswordEyeBtn.bezelStyle = NSBezelStyleRounded;
            [cv addSubview:sncPasswordEyeBtn];

            NSButton *loginBtn = [NSButton buttonWithTitle:@"Login"
                                                     target:sncLoginDelegate
                                                     action:@selector(loginClicked:)];
            loginBtn.frame = NSMakeRect(20, 20, 120, 32);
            loginBtn.keyEquivalent = @"\r";
            [cv addSubview:loginBtn];

            NSButton *cancelBtn = [NSButton buttonWithTitle:@"Cancel"
                                                      target:sncLoginDelegate
                                                      action:@selector(cancelClicked:)];
            cancelBtn.frame = NSMakeRect(150, 20, 120, 32);
            [cv addSubview:cancelBtn];

            NSButton *keyModeBtn = [NSButton buttonWithTitle:@"I Have a Key"
                                                       target:sncLoginDelegate
                                                       action:@selector(keyModeClicked:)];
            keyModeBtn.frame = NSMakeRect(280, 20, 140, 32);
            [cv addSubview:keyModeBtn];

            [sncLoginPanel makeKeyAndOrderFront:nil];
            [sncLoginPanel makeFirstResponder:sncEmailField];
        }
    });
}

// ── Native NSApp menu bar ─────────────────────────────────────────────────────
//
// Mirrors Windows' native menu bar (uiwindow.go createMenuBar) so every
// action is reachable from the window without hunting for the tray icon.
// Built once via snc_window_build_app_menu(); checkmarks kept in sync via
// snc_window_sync_app_menu() which is called from Go whenever settings change.

static NSMenuItem *sncMenuDoH;
static NSMenuItem *sncMenuQUIC;
static NSMenuItem *sncMenuRegionAuto;
static NSMenuItem *sncMenuRegionRU;
static NSMenuItem *sncMenuRegionEU;
static NSMenuItem *sncMenuRegionUS;
static NSMenuItem *sncMenuRegionCN;
static NSMenuItem *sncMenuRegionXX;
static NSMenuItem *sncMenuUpdate;

@interface SNCMenuBarDelegate : NSObject
@end

@implementation SNCMenuBarDelegate
- (void)menuLogin:(id)sender      { go_snc_menu_login(); }
- (void)menuLogout:(id)sender     { go_snc_menu_logout(); }
- (void)menuConnect:(id)sender    { go_snc_menu_connect(); }
- (void)menuDisconnect:(id)sender { go_snc_menu_disconnect(); }
- (void)menuToggleDoH:(id)sender  { go_snc_menu_toggle_doh(); }
- (void)menuToggleQUIC:(id)sender { go_snc_menu_toggle_quic(); }
- (void)menuRegion:(id)sender {
    NSString *code = [(NSMenuItem *)sender representedObject];
    go_snc_menu_region(code ? [code UTF8String] : "");
}
- (void)menuAbout:(id)sender  { go_snc_menu_about(); }
- (void)menuUpdate:(id)sender { go_snc_menu_update(); }
- (void)menuQuit:(id)sender   { go_snc_menu_quit(); }
@end

static SNCMenuBarDelegate *sncMenuDelegate;

static NSMenuItem *makeItem(NSMenu *menu, NSString *title, SEL action) {
    NSMenuItem *item = [[NSMenuItem alloc]
        initWithTitle:title action:action keyEquivalent:@""];
    [item setTarget:sncMenuDelegate];
    [menu addItem:item];
    return item;
}

void snc_window_build_app_menu(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        @autoreleasepool {
            sncMenuDelegate = [[SNCMenuBarDelegate alloc] init];

            NSMenu *menuBar = [[NSMenu alloc] initWithTitle:@""];
            [NSApp setMainMenu:menuBar];

            // Single top-level "ShortNerdCat" popup.
            NSMenuItem *topItem = [[NSMenuItem alloc] initWithTitle:@"ShortNerdCat"
                                                             action:nil
                                                      keyEquivalent:@""];
            [menuBar addItem:topItem];
            NSMenu *appMenu = [[NSMenu alloc] initWithTitle:@"ShortNerdCat"];
            [topItem setSubmenu:appMenu];

            makeItem(appMenu, @"Login",      @selector(menuLogin:));
            makeItem(appMenu, @"Logout",     @selector(menuLogout:));
            [appMenu addItem:[NSMenuItem separatorItem]];
            makeItem(appMenu, @"Connect",    @selector(menuConnect:));
            makeItem(appMenu, @"Disconnect", @selector(menuDisconnect:));
            [appMenu addItem:[NSMenuItem separatorItem]];
            sncMenuDoH     = makeItem(appMenu, @"DNS over HTTPS", @selector(menuToggleDoH:));
            sncMenuQUIC    = makeItem(appMenu, @"Disable QUIC",   @selector(menuToggleQUIC:));

            // Region submenu.
            NSMenuItem *regionTop = [[NSMenuItem alloc] initWithTitle:@"Region"
                                                               action:nil
                                                        keyEquivalent:@""];
            [appMenu addItem:regionTop];
            NSMenu *regionMenu = [[NSMenu alloc] initWithTitle:@"Region"];
            [regionTop setSubmenu:regionMenu];

            NSArray *regionItems = @[
                @[@"Auto",   @""],
                @[@"Russia", @"RU"],
                @[@"Europe", @"EU"],
                @[@"USA",    @"US"],
                @[@"China",  @"CN"],
                @[@"Other",  @"XX"],
            ];
            NSMutableArray *sncRegionMenuItems = [NSMutableArray array];
            for (NSArray *pair in regionItems) {
                NSMenuItem *it = [[NSMenuItem alloc] initWithTitle:pair[0]
                                                            action:@selector(menuRegion:)
                                                     keyEquivalent:@""];
                [it setTarget:sncMenuDelegate];
                [it setRepresentedObject:pair[1]];
                [regionMenu addItem:it];
                [sncRegionMenuItems addObject:it];
            }
            sncMenuRegionAuto = sncRegionMenuItems[0];
            sncMenuRegionRU   = sncRegionMenuItems[1];
            sncMenuRegionEU   = sncRegionMenuItems[2];
            sncMenuRegionUS   = sncRegionMenuItems[3];
            sncMenuRegionCN   = sncRegionMenuItems[4];
            sncMenuRegionXX   = sncRegionMenuItems[5];

            [appMenu addItem:[NSMenuItem separatorItem]];
            makeItem(appMenu, @"About", @selector(menuAbout:));
            sncMenuUpdate = makeItem(appMenu, @"Update available", @selector(menuUpdate:));
            [sncMenuUpdate setEnabled:NO];
            [appMenu addItem:[NSMenuItem separatorItem]];
            makeItem(appMenu, @"Quit ShortNerdCat", @selector(menuQuit:));
        }
    });
}

void snc_window_sync_app_menu(int doh, int quic, const char *region, int updateReady) {
    dispatch_async(dispatch_get_main_queue(), ^{
        @autoreleasepool {
            if (!sncMenuDoH) return; // not built yet
            sncMenuDoH.state     = doh     ? NSControlStateValueOn : NSControlStateValueOff;
            sncMenuQUIC.state    = quic    ? NSControlStateValueOn : NSControlStateValueOff;
            [sncMenuQUIC setEnabled:YES];
            [sncMenuUpdate setEnabled:updateReady ? YES : NO];

            NSString *code = region ? [NSString stringWithUTF8String:region] : @"";
            sncMenuRegionAuto.state = ([code isEqualToString:@""])   ? NSControlStateValueOn : NSControlStateValueOff;
            sncMenuRegionRU.state   = ([code isEqualToString:@"RU"]) ? NSControlStateValueOn : NSControlStateValueOff;
            sncMenuRegionEU.state   = ([code isEqualToString:@"EU"]) ? NSControlStateValueOn : NSControlStateValueOff;
            sncMenuRegionUS.state   = ([code isEqualToString:@"US"]) ? NSControlStateValueOn : NSControlStateValueOff;
            sncMenuRegionCN.state   = ([code isEqualToString:@"CN"]) ? NSControlStateValueOn : NSControlStateValueOff;
            sncMenuRegionXX.state   = ([code isEqualToString:@"XX"]) ? NSControlStateValueOn : NSControlStateValueOff;
        }
    });
}

// ── navlink:// URL scheme handler ─────────────────────────────────────────────

@interface SNCURLHandler : NSObject
- (void)handleGetURLEvent:(NSAppleEventDescriptor *)event
           withReplyEvent:(NSAppleEventDescriptor *)reply;
@end

@implementation SNCURLHandler
- (void)handleGetURLEvent:(NSAppleEventDescriptor *)event
           withReplyEvent:(NSAppleEventDescriptor *)reply {
    NSString *urlStr = [[event paramDescriptorForKeyword:keyDirectObject] stringValue];
    if (urlStr) {
        goNavlinkURL([urlStr UTF8String]);
    }
}
@end

static SNCURLHandler *sncURLHandler;

void snc_register_url_handler(void) {
    sncURLHandler = [[SNCURLHandler alloc] init];
    [[NSAppleEventManager sharedAppleEventManager]
        setEventHandler:sncURLHandler
            andSelector:@selector(handleGetURLEvent:withReplyEvent:)
          forEventClass:kInternetEventClass
             andEventID:kAEGetURL];
}
