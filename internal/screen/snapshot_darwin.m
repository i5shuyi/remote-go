// Development aid: write each of the app's own windows to a PNG, as the
// window server composites it (video layers included). Capturing one's own
// windows needs no screen-recording permission.

#import <Cocoa/Cocoa.h>
#include <dlfcn.h>

typedef CGImageRef (*windowImageFn)(CGRect, uint32_t, uint32_t, uint32_t);

void rg_snapshot(const char *dir) {
	NSString *d = [NSString stringWithUTF8String:dir];
	dispatch_async(dispatch_get_main_queue(), ^{
		windowImageFn f = (windowImageFn)dlsym(RTLD_DEFAULT, "CGWindowListCreateImage");
		if (!f) { NSLog(@"snapshot: CGWindowListCreateImage unavailable"); return; }
		int i = 0;
		for (NSWindow *w in NSApp.windows) {
			if (!w.visible) continue;
			// kCGWindowListOptionIncludingWindow = 8, kCGWindowImageBoundsIgnoreFraming = 1
			CGImageRef img = f(CGRectNull, 8, (uint32_t)w.windowNumber, 1);
			if (!img) continue;
			NSBitmapImageRep *rep = [[NSBitmapImageRep alloc] initWithCGImage:img];
			CGImageRelease(img);
			NSData *png = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
			NSString *p = [d stringByAppendingPathComponent:[NSString stringWithFormat:@"window-%d.png", i++]];
			[png writeToFile:p atomically:YES];
			NSLog(@"snapshot: %@ → %@", w.title, p);
		}
	});
}
