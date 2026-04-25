#include <Cocoa/Cocoa.h>
#include <Carbon/Carbon.h>

extern void goAutoConvertToggle(void);
extern void goTrayQuit(void);

static NSStatusItem *statusItem = nil;
static NSMenu *statusMenu = nil;
static NSMenuItem *autoConvertItem = nil;

// Mirror of the Go-side autoConvertEnabled atomic. Updated only via
// updateAutoConvertMenu so the menu checkmark and the in-process flag
// stay in sync. We need a C-level mirror because ensureApp() may rebuild
// the menu after updateAutoConvertMenu has already been called once at
// startup with the persisted config value.
static int autoConvertOn = 1;

@class LayoutObserver;
static LayoutObserver *layoutObserver = nil;
static void setTrayTitleFromCurrentLayout(void);

@interface TrayDelegate : NSObject
- (void)autoConvertAction:(id)sender;
- (void)quitAction:(id)sender;
@end

@implementation TrayDelegate
- (void)autoConvertAction:(id)sender {
    goAutoConvertToggle();
}
- (void)quitAction:(id)sender {
    goTrayQuit();
}
@end

static TrayDelegate *delegate = nil;

@interface LayoutObserver : NSObject
- (void)layoutChanged:(NSNotification *)note;
@end

@implementation LayoutObserver
- (void)layoutChanged:(NSNotification *)note {
    setTrayTitleFromCurrentLayout();
}
@end

// setTrayTitleFromCurrentLayout reads the current keyboard input source
// (via TIS) and sets the tray title to the matching flag emoji. Always
// dispatches the UI mutation onto the main queue so it is safe to call
// from notification callbacks delivered on background threads.
static void setTrayTitleFromCurrentLayout(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (!statusItem || !statusItem.button) return;

        TISInputSourceRef src = TISCopyCurrentKeyboardInputSource();
        BOOL isRussian = NO;
        if (src) {
            CFStringRef sid = TISGetInputSourceProperty(src, kTISPropertyInputSourceID);
            if (sid) {
                CFRange r = CFStringFind(sid, CFSTR("Russian"), kCFCompareCaseInsensitive);
                if (r.location != kCFNotFound) isRussian = YES;
            }
            CFRelease(src);
        }
        statusItem.button.title = isRussian ? @"🇷🇺" : @"🇺🇸";
    });
}

// updateAutoConvertMenu syncs the tray menu checkmark with the Go-side
// flag. Safe to call before the menu exists (guarded against nil).
void updateAutoConvertMenu(int enabled) {
    autoConvertOn = enabled;
    dispatch_async(dispatch_get_main_queue(), ^{
        if (autoConvertItem) {
            autoConvertItem.state = enabled ? NSControlStateValueOn
                                            : NSControlStateValueOff;
        }
    });
}

void removeTray(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (statusItem) {
            [[NSStatusBar systemStatusBar] removeStatusItem:statusItem];
            statusItem = nil;
        }
    });
}

void ensureApp(void) {
    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];

    if (!delegate) {
        delegate = [[TrayDelegate alloc] init];
    }

    statusItem = [[NSStatusBar systemStatusBar]
        statusItemWithLength:NSVariableStatusItemLength];
    // System font at 14pt; Apple Color Emoji fallback renders the regional
    // indicator pair as a single flag glyph.
    statusItem.button.font = [NSFont systemFontOfSize:14];

    // Initial probe — keep the first paint correct without waiting for
    // the layout-change notification to fire.
    {
        TISInputSourceRef src = TISCopyCurrentKeyboardInputSource();
        BOOL isRussian = NO;
        if (src) {
            CFStringRef sid = TISGetInputSourceProperty(src, kTISPropertyInputSourceID);
            if (sid) {
                CFRange r = CFStringFind(sid, CFSTR("Russian"), kCFCompareCaseInsensitive);
                if (r.location != kCFNotFound) isRussian = YES;
            }
            CFRelease(src);
        }
        statusItem.button.title = isRussian ? @"🇷🇺" : @"🇺🇸";
    }

    statusMenu = [[NSMenu alloc] init];

    autoConvertItem = [[NSMenuItem alloc] initWithTitle:@"Автоконвертация"
                                                 action:@selector(autoConvertAction:)
                                          keyEquivalent:@""];
    autoConvertItem.target = delegate;
    autoConvertItem.state = autoConvertOn ? NSControlStateValueOn
                                          : NSControlStateValueOff;
    [statusMenu addItem:autoConvertItem];

    [statusMenu addItem:[NSMenuItem separatorItem]];

    NSMenuItem *quitItem = [[NSMenuItem alloc] initWithTitle:@"Выйти"
                                                      action:@selector(quitAction:)
                                               keyEquivalent:@"q"];
    quitItem.target = delegate;
    [statusMenu addItem:quitItem];

    statusItem.menu = statusMenu;

    // Subscribe to keyboard layout changes. The TIS notification is the
    // canonical signal; we also register the AppleSelectedInputSources
    // legacy name in case it is delivered first in some app contexts.
    if (!layoutObserver) {
        layoutObserver = [[LayoutObserver alloc] init];
    }
    NSDistributedNotificationCenter *dnc =
        [NSDistributedNotificationCenter defaultCenter];
    [dnc addObserver:layoutObserver
            selector:@selector(layoutChanged:)
                name:(NSString *)kTISNotifySelectedKeyboardInputSourceChanged
              object:nil];
    [dnc addObserver:layoutObserver
            selector:@selector(layoutChanged:)
                name:@"AppleSelectedInputSourcesChangedNotification"
              object:nil];
}

void runNSApp(void) {
    // Use [NSApp run] — the standard way. This processes menu events
    // correctly. CGEventTap runs on its own thread via startHook(), so no
    // conflict.
    [NSApp run];
}
