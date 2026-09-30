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

// Battery temperature comes from the private IOHIDEventSystem API that
// macmon, Stats and iStat Menus also use: the fuel gauge publishes
// "gas gauge battery" temperature services (usage page 0xff00, usage 5)
// readable without root.
#include <IOKit/hidsystem/IOHIDEventSystemClient.h>

typedef struct __IOHIDEvent *IOHIDEventRef;
typedef struct __IOHIDServiceClient *IOHIDServiceClientRef;
IOHIDEventSystemClientRef IOHIDEventSystemClientCreate(CFAllocatorRef allocator);
int IOHIDEventSystemClientSetMatching(IOHIDEventSystemClientRef client, CFDictionaryRef match);
IOHIDEventRef IOHIDServiceClientCopyEvent(IOHIDServiceClientRef service, int64_t type, int32_t options, int64_t timestamp);
CFTypeRef IOHIDServiceClientCopyProperty(IOHIDServiceClientRef service, CFStringRef key);
double IOHIDEventGetFloatValue(IOHIDEventRef event, int32_t field);

enum { kTemperatureEvent = 15 };

// bckp_battery_temps writes up to max battery sensor readings (°C) to out
// and returns how many it wrote. Filtering implausible values is the Go
// side's job.
int bckp_battery_temps(double *out, int max) {
	int n = 0;
	int page = 0xff00, usage = 5;
	CFNumberRef p = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &page);
	CFNumberRef u = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &usage);
	const void *keys[] = {CFSTR("PrimaryUsagePage"), CFSTR("PrimaryUsage")};
	const void *vals[] = {p, u};
	CFDictionaryRef match = CFDictionaryCreate(kCFAllocatorDefault, keys, vals, 2,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFRelease(p);
	CFRelease(u);
	IOHIDEventSystemClientRef client = IOHIDEventSystemClientCreate(kCFAllocatorDefault);
	if (client == NULL) {
		CFRelease(match);
		return 0;
	}
	IOHIDEventSystemClientSetMatching(client, match);
	CFRelease(match);
	CFArrayRef services = IOHIDEventSystemClientCopyServices(client);
	if (services != NULL) {
		for (CFIndex i = 0; i < CFArrayGetCount(services) && n < max; i++) {
			IOHIDServiceClientRef svc = (IOHIDServiceClientRef)CFArrayGetValueAtIndex(services, i);
			CFTypeRef name = IOHIDServiceClientCopyProperty(svc, CFSTR("Product"));
			bool battery = name != NULL && CFGetTypeID(name) == CFStringGetTypeID() &&
				CFStringCompare((CFStringRef)name, CFSTR("gas gauge battery"), 0) == kCFCompareEqualTo;
			if (name != NULL) {
				CFRelease(name);
			}
			if (!battery) {
				continue;
			}
			IOHIDEventRef ev = IOHIDServiceClientCopyEvent(svc, kTemperatureEvent, 0, 0);
			if (ev != NULL) {
				out[n++] = IOHIDEventGetFloatValue(ev, kTemperatureEvent << 16);
				CFRelease(ev);
			}
		}
		CFRelease(services);
	}
	CFRelease(client);
	return n;
}
