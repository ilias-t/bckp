//go:build darwin && cgo

#import <Foundation/Foundation.h>

// bckp_thermal_state returns NSProcessInfo.thermalState (0 nominal .. 3
// critical). It needs no window server or user session, so it is expected
// to work in a root LaunchDaemon (not yet verified as root).
int bckp_thermal_state(void) {
	@autoreleasepool {
		return (int)[[NSProcessInfo processInfo] thermalState];
	}
}
