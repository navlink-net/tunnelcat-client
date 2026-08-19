// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

#include <IOKit/pwr_mgt/IOPMLib.h>
#include <IOKit/IOMessage.h>
#include <CoreFoundation/CoreFoundation.h>

static io_connect_t sncRootPort = 0;

// Forward-declare the Go-exported callback using primitive types that match
// CGO's mapping: io_service_t → unsigned int, natural_t → unsigned int.
extern void sncPowerCallback(void *refcon, unsigned int service,
                              unsigned int messageType, void *messageArgument);

void startPowerWatcher(void) {
    IONotificationPortRef notifyPort;
    io_object_t           notifier;

    sncRootPort = IORegisterForSystemPower(
        NULL, &notifyPort, (IOServiceInterestCallback)sncPowerCallback, &notifier);
    if (sncRootPort == 0) {
        return;
    }

    CFRunLoopAddSource(
        CFRunLoopGetCurrent(),
        IONotificationPortGetRunLoopSource(notifyPort),
        kCFRunLoopDefaultMode);

    CFRunLoopRun();
}

void allowPowerChange(long notificationID) {
    if (sncRootPort != 0) {
        IOAllowPowerChange(sncRootPort, notificationID);
    }
}
