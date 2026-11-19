// macOS remote-screen view: an NSView hosting an AVSampleBufferDisplayLayer
// (VideoToolbox hardware decode, composited without copies) and a plain
// CALayer for JPEG frames. It forwards raw input to Go.

#import <Cocoa/Cocoa.h>
#import <AVFoundation/AVFoundation.h>
#import <CoreMedia/CoreMedia.h>
#import <ImageIO/ImageIO.h>
#include "screen_darwin.h"

extern void rgScreenEvent(uintptr_t h, int kind, int a, double x, double y);

extern void rgBallLayout(uintptr_t h, double width, double height, int move, int snap, double *x, double *y);

enum { evMove = 1, evDown, evUp, evWheel, evKeyDown, evKeyUp, evBlur, evControls };

#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"

@interface RGScreenView : NSView
- (instancetype)initWithHandle:(uintptr_t)h;
- (void)detach;
- (void)focusScreen;
- (int)h264:(const uint8_t *)d length:(int)n key:(BOOL)key;
- (void)jpeg:(const uint8_t *)d length:(int)n;
- (void)resetDecoder;
@end

// Keep pointer tracking in the button so a drag never reaches remote input.
@interface RGControlsButton : NSButton
@property (nonatomic, copy) void (^place)(NSPoint origin, BOOL snap);
- (void)cancelTracking;
@end

@implementation RGControlsButton {
	NSPoint _down, _grab;
	BOOL _tracking, _dragging;
}
- (void)mouseDown:(NSEvent *)e {
	_down = [self.superview convertPoint:e.locationInWindow fromView:nil];
	_grab = NSMakePoint(_down.x - NSMinX(self.frame), _down.y - NSMinY(self.frame));
	_tracking = YES;
	_dragging = NO;
}
- (void)track:(NSEvent *)e released:(BOOL)released {
	if (!_tracking) return;
	NSPoint p = [self.superview convertPoint:e.locationInWindow fromView:nil];
	if (hypot(p.x - _down.x, p.y - _down.y) >= 6) _dragging = YES;
	if (_dragging && self.place) self.place(NSMakePoint(p.x - _grab.x, p.y - _grab.y), released);
	if (released) {
		_tracking = NO;
		if (!_dragging && NSPointInRect(p, self.frame)) [self performClick:nil];
	}
}
- (void)mouseDragged:(NSEvent *)e { [self track:e released:NO]; }
- (void)mouseUp:(NSEvent *)e { [self track:e released:YES]; }
- (void)cancelTracking { _tracking = NO; }
@end

@implementation RGScreenView {
	uintptr_t _h;
	AVSampleBufferDisplayLayer *_vl;
	CALayer *_il;
	CMVideoFormatDescriptionRef _fmt;
	NSData *_sps, *_pps;
	CGSize _vsize; // guarded by @synchronized(self)
	NSTrackingArea *_ta;
	RGControlsButton *_controls;
	NSMutableIndexSet *_commandKeys;
	id _keyUpMonitor;
}

- (instancetype)initWithHandle:(uintptr_t)h {
	if (!(self = [super initWithFrame:NSMakeRect(0, 0, 640, 360)])) return nil;
	_h = h;
	CALayer *root = [CALayer layer];
	root.backgroundColor = CGColorGetConstantColor(kCGColorBlack);
	self.layer = root;
	self.wantsLayer = YES;
	_vl = [AVSampleBufferDisplayLayer new];
	_vl.videoGravity = AVLayerVideoGravityResizeAspect;
	_vl.backgroundColor = CGColorGetConstantColor(kCGColorBlack);
	_il = [CALayer layer];
	_il.contentsGravity = kCAGravityResizeAspect;
	_il.hidden = YES;
	_il.minificationFilter = kCAFilterTrilinear;
	[root addSublayer:_vl];
	[root addSublayer:_il];

	_controls = [RGControlsButton buttonWithImage:[NSImage imageWithSystemSymbolName:@"ellipsis" accessibilityDescription:nil]
	                                target:self action:@selector(openControls:)];
	_controls.frame = NSMakeRect(0, 0, 40, 40);
	__weak RGScreenView *weakSelf = self;
	_controls.place = ^(NSPoint origin, BOOL snap) {
		[weakSelf layoutControlsAt:origin move:YES snap:snap];
	};
	[self layoutControlsAt:NSZeroPoint move:NO snap:NO];
	_controls.bordered = NO;
	_controls.wantsLayer = YES;
	_controls.layer.cornerRadius = 20;
	_controls.layer.backgroundColor = [NSColor colorWithWhite:0.16 alpha:0.94].CGColor;
	_controls.layer.borderWidth = 1;
	_controls.layer.borderColor = [NSColor colorWithWhite:1 alpha:0.55].CGColor;
	_controls.contentTintColor = NSColor.whiteColor;
	_controls.toolTip = @"会话控制";
	[_controls setAccessibilityLabel:@"会话控制"];
	[self addSubview:_controls];
	_commandKeys = [NSMutableIndexSet indexSet];
	return self;
}

- (void)dealloc {
	if (_fmt) CFRelease(_fmt);
}

- (void)detach {
	[self stopWatchingWindow];
	if (_keyUpMonitor) [NSEvent removeMonitor:_keyUpMonitor];
	_keyUpMonitor = nil;
	_h = 0;
	[_vl flushAndRemoveImage];
}

- (void)stopWatchingWindow {
	NSNotificationCenter *center = NSNotificationCenter.defaultCenter;
	[center removeObserver:self name:NSWindowDidBecomeKeyNotification object:nil];
	[center removeObserver:self name:NSWindowDidResignKeyNotification object:nil];
}

- (void)viewDidMoveToWindow {
	[super viewDidMoveToWindow];
	NSNotificationCenter *center = NSNotificationCenter.defaultCenter;
	[self stopWatchingWindow];
	if (!self.window || !_h) return;
	[center addObserver:self selector:@selector(windowActive:) name:NSWindowDidBecomeKeyNotification object:self.window];
	[center addObserver:self selector:@selector(windowInactive:) name:NSWindowDidResignKeyNotification object:self.window];
	[self focusScreen];

	// AppKit can bypass the first responder on key-up for Command shortcuts.
	if (!_keyUpMonitor) {
		__weak RGScreenView *weakSelf = self;
		_keyUpMonitor = [NSEvent addLocalMonitorForEventsMatchingMask:NSEventMaskKeyUp handler:^NSEvent *(NSEvent *e) {
			RGScreenView *v = weakSelf;
			if (v && v.window == e.window && [v->_commandKeys containsIndex:e.keyCode]) {
				[v keyUp:e];
				return nil;
			}
			return e;
		}];
	}
}

- (void)focusScreen {
	if (_h && self.window.isKeyWindow) [self.window makeFirstResponder:self];
}

- (void)windowActive:(NSNotification *)note { [self focusScreen]; }

- (void)windowInactive:(NSNotification *)note { [_controls cancelTracking]; [self releaseKeys]; }

- (void)releaseKeys {
	[_commandKeys removeAllIndexes];
	[self emit:evBlur a:0 x:0 y:0];
}

- (void)openControls:(id)sender {
	[self releaseKeys];
	NSPoint at = [self convertPoint:NSMakePoint(NSMinX(_controls.frame), NSMaxY(_controls.frame) + 4)
	                         toView:self.window.contentView];
	[self emit:evControls a:0 x:at.x y:at.y];
}

- (BOOL)isFlipped { return YES; }
- (BOOL)acceptsFirstResponder { return YES; }
- (BOOL)acceptsFirstMouse:(NSEvent *)e { return YES; }
- (BOOL)mouseDownCanMoveWindow { return NO; }

- (void)layoutControlsAt:(NSPoint)origin move:(BOOL)move snap:(BOOL)snap {
	if (!_h || !_controls) return;
	double x = origin.x, y = origin.y;
	rgBallLayout(_h, NSWidth(self.bounds), NSHeight(self.bounds), move, snap, &x, &y);
	[_controls setFrameOrigin:NSMakePoint(x, y)];
}

- (void)setFrameSize:(NSSize)size {
	[super setFrameSize:size];
	[self layoutControlsAt:NSZeroPoint move:NO snap:NO];
	[CATransaction begin];
	[CATransaction setDisableActions:YES];
	_vl.frame = self.bounds;
	_il.frame = self.bounds;
	[CATransaction commit];
}

- (void)viewDidChangeBackingProperties {
	[super viewDidChangeBackingProperties];
	CGFloat s = self.window ? self.window.backingScaleFactor : 2;
	_il.contentsScale = s;
	_vl.contentsScale = s;
}

// ------------------------------------------------------------------ input

- (void)emit:(int)kind a:(int)a x:(double)x y:(double)y {
	if (_h) rgScreenEvent(_h, kind, a, x, y);
}

- (NSPoint)norm:(NSEvent *)e {
	NSPoint p = [self convertPoint:e.locationInWindow fromView:nil];
	NSSize b = self.bounds.size;
	CGSize v;
	@synchronized(self) { v = _vsize; }
	if (v.width <= 0 || v.height <= 0) v = b;
	CGFloat s = MIN(b.width / v.width, b.height / v.height);
	CGFloat w = v.width * s, h = v.height * s;
	if (w <= 0 || h <= 0) return NSMakePoint(0, 0);
	return NSMakePoint((p.x - (b.width - w) / 2) / w, (p.y - (b.height - h) / 2) / h);
}

- (void)updateTrackingAreas {
	[super updateTrackingAreas];
	if (_ta) [self removeTrackingArea:_ta];
	_ta = [[NSTrackingArea alloc] initWithRect:NSZeroRect
	                                   options:NSTrackingMouseMoved | NSTrackingActiveInKeyWindow | NSTrackingInVisibleRect
	                                     owner:self
	                                  userInfo:nil];
	[self addTrackingArea:_ta];
}

static int buttonOf(NSEvent *e) {
	switch (e.type) {
	case NSEventTypeLeftMouseDown: case NSEventTypeLeftMouseUp: case NSEventTypeLeftMouseDragged: return 0;
	case NSEventTypeRightMouseDown: case NSEventTypeRightMouseUp: case NSEventTypeRightMouseDragged: return 2;
	default: return e.buttonNumber == 2 ? 1 : (int)e.buttonNumber;
	}
}

- (void)press:(NSEvent *)e down:(BOOL)down {
	if (down) [self.window makeFirstResponder:self];
	NSPoint p = [self norm:e];
	[self emit:(down ? evDown : evUp) a:buttonOf(e) x:p.x y:p.y];
}

- (void)moved:(NSEvent *)e {
	NSPoint p = [self norm:e];
	[self emit:evMove a:0 x:p.x y:p.y];
}

- (void)mouseDown:(NSEvent *)e { [self press:e down:YES]; }
- (void)mouseUp:(NSEvent *)e { [self press:e down:NO]; }
- (void)rightMouseDown:(NSEvent *)e { [self press:e down:YES]; }
- (void)rightMouseUp:(NSEvent *)e { [self press:e down:NO]; }
- (void)otherMouseDown:(NSEvent *)e { [self press:e down:YES]; }
- (void)otherMouseUp:(NSEvent *)e { [self press:e down:NO]; }
- (void)mouseMoved:(NSEvent *)e { [self moved:e]; }
- (void)mouseDragged:(NSEvent *)e { [self moved:e]; }
- (void)rightMouseDragged:(NSEvent *)e { [self moved:e]; }
- (void)otherMouseDragged:(NSEvent *)e { [self moved:e]; }
- (NSMenu *)menuForEvent:(NSEvent *)e { return nil; }

- (void)scrollWheel:(NSEvent *)e {
	double dx = e.scrollingDeltaX, dy = e.scrollingDeltaY;
	if (!e.hasPreciseScrollingDeltas) { dx *= 33; dy *= 33; }
	// Browser convention: positive dy scrolls the content down.
	[self emit:evWheel a:0 x:-dx y:-dy];
}

- (void)keyDown:(NSEvent *)e { [self emit:evKeyDown a:e.keyCode x:0 y:0]; }
- (void)keyUp:(NSEvent *)e {
	[_commandKeys removeIndex:e.keyCode];
	[self emit:evKeyUp a:e.keyCode x:0 y:0];
}

// Command shortcuts arrive here before the application menu. Cmd+Q stays local.
- (BOOL)performKeyEquivalent:(NSEvent *)e {
	if (self.window.firstResponder != self || e.type != NSEventTypeKeyDown) return NO;
	NSEventModifierFlags f = e.modifierFlags & NSEventModifierFlagDeviceIndependentFlagsMask;
	if (!(f & NSEventModifierFlagCommand)) return NO;
	if (f == NSEventModifierFlagCommand && [e.charactersIgnoringModifiers isEqualToString:@"q"]) return NO;
	[_commandKeys addIndex:e.keyCode];
	[self keyDown:e];
	return YES;
}

static NSUInteger deviceMask(unsigned short kc) {
	switch (kc) {
	case 0x3B: return 0x0001;   // left control
	case 0x38: return 0x0002;   // left shift
	case 0x3C: return 0x0004;   // right shift
	case 0x37: return 0x0008;   // left command
	case 0x36: return 0x0010;   // right command
	case 0x3A: return 0x0020;   // left option
	case 0x3D: return 0x0040;   // right option
	case 0x3E: return 0x2000;   // right control
	}
	return 0;
}

- (void)flagsChanged:(NSEvent *)e {
	unsigned short kc = e.keyCode;
	if (kc == 0x39) { // Caps Lock reports only its toggled state: tap it.
		[self emit:evKeyDown a:kc x:0 y:0];
		[self emit:evKeyUp a:kc x:0 y:0];
		return;
	}
	NSUInteger m = deviceMask(kc);
	if (!m) return;
	[self emit:((e.modifierFlags & m) ? evKeyDown : evKeyUp) a:kc x:0 y:0];
}

- (BOOL)resignFirstResponder {
	[self releaseKeys];
	return [super resignFirstResponder];
}

// ------------------------------------------------------------------ video

- (void)showVideo:(BOOL)video {
	dispatch_async(dispatch_get_main_queue(), ^{
		[CATransaction begin];
		[CATransaction setDisableActions:YES];
		self->_vl.hidden = !video;
		self->_il.hidden = video;
		if (video) self->_il.contents = nil;
		[CATransaction commit];
	});
}

- (void)resetDecoder {
	[_vl flush];
	if (_fmt) { CFRelease(_fmt); _fmt = NULL; }
	_sps = _pps = nil;
}

// h264 enqueues one Annex-B access unit. It returns -1 when the decoder
// needs a keyframe to continue.
- (int)h264:(const uint8_t *)d length:(int)n key:(BOOL)key {
	// Split into NAL units.
	NSMutableData *avcc = [NSMutableData dataWithCapacity:n + 16];
	NSData *sps = nil, *pps = nil;
	int i = 0, start = -1;
	for (;;) {
		int next = -1, sc = 0;
		for (int j = i; j + 3 <= n; j++) {
			if (d[j] == 0 && d[j + 1] == 0 && (d[j + 2] == 1 || (j + 4 <= n && d[j + 2] == 0 && d[j + 3] == 1))) {
				next = j;
				sc = d[j + 2] == 1 ? 3 : 4;
				break;
			}
		}
		if (start >= 0) {
			int end = next < 0 ? n : next;
			while (end > start && d[end - 1] == 0) end--;
			if (end > start) {
				int type = d[start] & 0x1f;
				if (type == 7) sps = [NSData dataWithBytes:d + start length:end - start];
				else if (type == 8) pps = [NSData dataWithBytes:d + start length:end - start];
				else if (type != 9) { // drop access unit delimiters
					uint32_t len = CFSwapInt32HostToBig((uint32_t)(end - start));
					[avcc appendBytes:&len length:4];
					[avcc appendBytes:d + start length:end - start];
				}
			}
		}
		if (next < 0) break;
		start = next + sc;
		i = start;
	}

	if (sps && pps && !(_fmt && [sps isEqualToData:_sps] && [pps isEqualToData:_pps])) {
		const uint8_t *ps[2] = {sps.bytes, pps.bytes};
		size_t sz[2] = {sps.length, pps.length};
		CMVideoFormatDescriptionRef f = NULL;
		if (CMVideoFormatDescriptionCreateFromH264ParameterSets(kCFAllocatorDefault, 2, ps, sz, 4, &f) != noErr) return -1;
		if (_fmt) {
			[_vl flush];
			CFRelease(_fmt);
		}
		_fmt = f;
		_sps = sps;
		_pps = pps;
		CMVideoDimensions dim = CMVideoFormatDescriptionGetDimensions(f);
		CGRect clean = CMVideoFormatDescriptionGetCleanAperture(f, true);
		@synchronized(self) {
			_vsize = clean.size.width > 0 ? clean.size : CGSizeMake(dim.width, dim.height);
		}
		[self showVideo:YES];
	}
	if (!_fmt) return -1;
	if (avcc.length == 0) return 0;

	if (_vl.status == AVQueuedSampleBufferRenderingStatusFailed) {
		[_vl flush];
		return -1;
	}

	CMBlockBufferRef bb = NULL;
	if (CMBlockBufferCreateWithMemoryBlock(kCFAllocatorDefault, NULL, avcc.length, kCFAllocatorDefault, NULL, 0,
	                                       avcc.length, 0, &bb) != kCMBlockBufferNoErr) return -1;
	CMBlockBufferReplaceDataBytes(avcc.bytes, bb, 0, avcc.length);
	CMSampleBufferRef sb = NULL;
	size_t size = avcc.length;
	OSStatus st = CMSampleBufferCreateReady(kCFAllocatorDefault, bb, _fmt, 1, 0, NULL, 1, &size, &sb);
	CFRelease(bb);
	if (st != noErr) return -1;
	CFArrayRef atts = CMSampleBufferGetSampleAttachmentsArray(sb, true);
	if (atts && CFArrayGetCount(atts) > 0) {
		CFMutableDictionaryRef a = (CFMutableDictionaryRef)CFArrayGetValueAtIndex(atts, 0);
		CFDictionarySetValue(a, kCMSampleAttachmentKey_DisplayImmediately, kCFBooleanTrue);
		if (!key) CFDictionarySetValue(a, kCMSampleAttachmentKey_NotSync, kCFBooleanTrue);
	}
	[_vl enqueueSampleBuffer:sb];
	CFRelease(sb);
	static int debug = -1, count;
	if (debug < 0) debug = getenv("RG_SCREEN_DEBUG") != NULL;
	if (debug && ++count % 30 == 0) {
		CGSize v;
		@synchronized(self) { v = _vsize; }
		NSLog(@"screen: %d frames enqueued, video %.0fx%.0f, layer status %ld, error %@, view %.0fx%.0f in window %@",
		      count, v.width, v.height, (long)_vl.status, _vl.error, self.bounds.size.width, self.bounds.size.height,
		      self.window ? @"yes" : @"no");
	}
	return 0;
}

- (void)jpeg:(const uint8_t *)d length:(int)n {
	CFDataRef data = CFDataCreate(kCFAllocatorDefault, d, n);
	CGImageSourceRef src = CGImageSourceCreateWithData(data, NULL);
	CFRelease(data);
	if (!src) return;
	CGImageRef img = CGImageSourceCreateImageAtIndex(src, 0, NULL);
	CFRelease(src);
	if (!img) return;
	@synchronized(self) {
		_vsize = CGSizeMake(CGImageGetWidth(img), CGImageGetHeight(img));
	}
	dispatch_async(dispatch_get_main_queue(), ^{
		[CATransaction begin];
		[CATransaction setDisableActions:YES];
		self->_il.contents = (__bridge id)img;
		self->_il.hidden = NO;
		self->_vl.hidden = YES;
		[CATransaction commit];
		CGImageRelease(img);
	});
}

@end

#pragma clang diagnostic pop

// ------------------------------------------------------------------ C API

void *rg_screen_new(uintptr_t h) {
	RGScreenView *v = [[RGScreenView alloc] initWithHandle:h];
	return (__bridge_retained void *)v;
}

void rg_screen_free(void *p) {
	RGScreenView *v = (__bridge_transfer RGScreenView *)p;
	[v detach];
	v = nil;
}

int rg_screen_h264(void *p, const uint8_t *d, int n, int key) {
	@autoreleasepool {
		return [(__bridge RGScreenView *)p h264:d length:n key:key != 0];
	}
}

void rg_screen_jpeg(void *p, const uint8_t *d, int n) {
	@autoreleasepool {
		[(__bridge RGScreenView *)p jpeg:d length:n];
	}
}

void rg_screen_reset(void *p) {
	@autoreleasepool {
		[(__bridge RGScreenView *)p resetDecoder];
	}
}

void rg_screen_focus(void *p) { [(__bridge RGScreenView *)p focusScreen]; }
