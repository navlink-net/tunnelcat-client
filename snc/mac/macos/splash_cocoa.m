// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build darwin
// +build darwin

// splash_cocoa.m — startup splash screen and About dialog for ShortNerdCat.
//
// Splash design mirrors the Windows version:
//   • fully transparent borderless window (logo floats on the desktop)
//   • logo.png scaled to 360 px tall, proportional width
//   • "Tunnel Cat" in bold white 42 pt with drop-shadow near the top
//   • version string in white 18 pt with drop-shadow near the bottom

#import <Cocoa/Cocoa.h>
#include "window_cocoa.h"

static NSWindow *sncSplashWindow = nil;

// ── helpers ───────────────────────────────────────────────────────────────────

static NSShadow *makeShadow(void) {
    NSShadow *s      = [[NSShadow alloc] init];
    s.shadowColor      = [NSColor colorWithWhite:0.0 alpha:0.80];
    s.shadowOffset     = NSMakeSize(2.0, -2.0);
    s.shadowBlurRadius = 4.0;
    return s;
}

static NSTextField *makeLabel(NSRect frame, NSAttributedString *attrStr) {
    NSTextField *lbl  = [[NSTextField alloc] initWithFrame:frame];
    lbl.bezeled         = NO;
    lbl.drawsBackground = NO;
    lbl.editable        = NO;
    lbl.selectable      = NO;
    lbl.attributedStringValue = attrStr;
    return lbl;
}

static NSMutableParagraphStyle *centred(void) {
    NSMutableParagraphStyle *ps = [[NSMutableParagraphStyle alloc] init];
    ps.alignment = NSTextAlignmentCenter;
    return ps;
}

// ── Splash ────────────────────────────────────────────────────────────────────

void snc_splash_open(const unsigned char *pngData, int pngLen, const char *versionStr) {
    NSData   *imgData = [NSData dataWithBytes:pngData length:(NSUInteger)pngLen];
    NSString *version = [NSString stringWithUTF8String:versionStr];

    dispatch_async(dispatch_get_main_queue(), ^{
        @autoreleasepool {
            // ── Scale logo to 360 px tall, keep aspect ratio ──────────────────
            NSImage *logo = [[NSImage alloc] initWithData:imgData];
            const CGFloat H = 360.0;
            CGFloat W = H;
            if (logo && logo.size.height > 0.0)
                W = ceil(H * logo.size.width / logo.size.height);

            // ── Centre on the primary screen's visible area ───────────────────
            NSScreen *screen = [NSScreen mainScreen] ?: [NSScreen screens].firstObject;
            NSRect sf = screen ? screen.visibleFrame : NSMakeRect(0, 0, 1440, 900);
            NSRect frame = NSMakeRect(
                sf.origin.x + floor((sf.size.width  - W) / 2.0),
                sf.origin.y + floor((sf.size.height - H) / 2.0),
                W, H);

            // ── Borderless, fully transparent, floating window ────────────────
            sncSplashWindow = [[NSWindow alloc]
                initWithContentRect:frame
                          styleMask:NSWindowStyleMaskBorderless
                            backing:NSBackingStoreBuffered
                              defer:NO];
            [sncSplashWindow setLevel:NSFloatingWindowLevel];
            [sncSplashWindow setOpaque:NO];
            [sncSplashWindow setBackgroundColor:[NSColor clearColor]];
            [sncSplashWindow setMovableByWindowBackground:YES];
            [sncSplashWindow setCollectionBehavior:
                NSWindowCollectionBehaviorMoveToActiveSpace];

            // Transparent content view
            NSView *cv = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, W, H)];
            cv.wantsLayer = YES;
            cv.layer.backgroundColor = [[NSColor clearColor] CGColor];
            [sncSplashWindow setContentView:cv];

            // ── Logo fills the entire window ──────────────────────────────────
            NSImageView *imgView = [[NSImageView alloc]
                initWithFrame:NSMakeRect(0, 0, W, H)];
            imgView.image        = logo;
            imgView.imageScaling = NSImageScaleProportionallyUpOrDown;
            [cv addSubview:imgView];

            NSShadow *sh = makeShadow();

            // ── "Tunnel Cat" near the top ─────────────────────────────────────
            const CGFloat titleH = 54.0, titleTopPad = 14.0;
            NSAttributedString *titleStr = [[NSAttributedString alloc]
                initWithString:@"Tunnel Cat"
                    attributes:@{
                        NSFontAttributeName:            [NSFont boldSystemFontOfSize:42],
                        NSForegroundColorAttributeName: [NSColor whiteColor],
                        NSShadowAttributeName:          sh,
                        NSKernAttributeName:            @(0.5),
                        NSParagraphStyleAttributeName:  centred(),
                    }];
            [cv addSubview:makeLabel(
                NSMakeRect(0, H - titleH - titleTopPad, W, titleH), titleStr)];

            // ── Version near the bottom ───────────────────────────────────────
            const CGFloat verH = 28.0, verBottomPad = 18.0;
            NSAttributedString *verStr = [[NSAttributedString alloc]
                initWithString:version
                    attributes:@{
                        NSFontAttributeName:            [NSFont systemFontOfSize:18],
                        NSForegroundColorAttributeName: [NSColor colorWithWhite:0.92 alpha:1.0],
                        NSShadowAttributeName:          sh,
                        NSParagraphStyleAttributeName:  centred(),
                    }];
            [cv addSubview:makeLabel(
                NSMakeRect(0, verBottomPad, W, verH), verStr)];

            // Show — become Regular so a root process can bring the window to front.
            [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
            [sncSplashWindow makeKeyAndOrderFront:nil];
            [NSApp activateIgnoringOtherApps:YES];
        }
    });
}

void snc_splash_close(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        @autoreleasepool {
            [sncSplashWindow close];
            sncSplashWindow = nil;
            [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        }
    });
}

