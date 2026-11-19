//go:build darwin && cgo

// Zero-copy hardware pipeline:
//   ScreenCaptureKit (GPU capture + GPU scaling, NV12 IOSurface)
//     → VideoToolbox VTCompressionSession (Apple media engine, H.264)
//     → Annex-B NAL units → Go
#import <Foundation/Foundation.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <VideoToolbox/VideoToolbox.h>
#include "vt_darwin.h"
#include "_cgo_export.h"

static void seterr(char *err, int errlen, NSString *msg) {
	if (err && errlen > 0) snprintf(err, errlen, "%s", msg.UTF8String ?: "unknown error");
}

@interface RGEncoder : NSObject <SCStreamOutput, SCStreamDelegate> {
@public
	uintptr_t handle;
	VTCompressionSessionRef vt;
	SCStream *stream;
	dispatch_queue_t q;
	CVPixelBufferRef last;
	CMTime lastPTS;
	BOOL forceKey;
	volatile BOOL stopped;
}
- (OSStatus)setupWidth:(int)w height:(int)h fps:(int)fps bitrate:(int)bitrate;
- (void)encode:(CVPixelBufferRef)pb pts:(CMTime)pts;
@end

// VideoToolbox output: AVCC (length-prefixed) → Annex-B (start codes), with
// SPS/PPS prepended on keyframes so the browser decoder can (re)start there.
static void vtOutput(void *refcon, void *frameRefcon, OSStatus status, VTEncodeInfoFlags flags,
                     CMSampleBufferRef sb) {
	if (status != noErr || sb == NULL || !CMSampleBufferDataIsReady(sb)) return;
	RGEncoder *e = (__bridge RGEncoder *)refcon;
	if (e->stopped) return;
	@autoreleasepool {
		BOOL key = YES;
		CFArrayRef atts = CMSampleBufferGetSampleAttachmentsArray(sb, false);
		if (atts && CFArrayGetCount(atts) > 0) {
			CFDictionaryRef d = CFArrayGetValueAtIndex(atts, 0);
			CFBooleanRef notSync = NULL;
			if (CFDictionaryGetValueIfPresent(d, kCMSampleAttachmentKey_NotSync, (const void **)&notSync) && notSync)
				key = !CFBooleanGetValue(notSync);
		}
		static const uint8_t sc[4] = {0, 0, 0, 1};
		NSMutableData *out = [NSMutableData dataWithCapacity:64 * 1024];
		CMFormatDescriptionRef fd = CMSampleBufferGetFormatDescription(sb);
		int nalLen = 4;
		size_t count = 0;
		CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fd, 0, NULL, NULL, &count, &nalLen);
		if (key) {
			for (size_t i = 0; i < count; i++) {
				const uint8_t *p; size_t sz;
				if (CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fd, i, &p, &sz, NULL, NULL) == noErr) {
					[out appendBytes:sc length:4];
					[out appendBytes:p length:sz];
				}
			}
		}
		CMBlockBufferRef bb = CMSampleBufferGetDataBuffer(sb);
		size_t len = CMBlockBufferGetDataLength(bb);
		NSMutableData *raw = [NSMutableData dataWithLength:len];
		if (CMBlockBufferCopyDataBytes(bb, 0, len, raw.mutableBytes) != kCMBlockBufferNoErr) return;
		const uint8_t *b = raw.bytes;
		size_t off = 0;
		while (off + nalLen <= len) {
			uint32_t l = 0;
			for (int i = 0; i < nalLen; i++) l = (l << 8) | b[off + i];
			off += nalLen;
			if (l == 0 || off + l > len) break;
			[out appendBytes:sc length:4];
			[out appendBytes:b + off length:l];
			off += l;
		}
		goOnFrame(e->handle, (void *)out.bytes, (int)out.length, key ? 1 : 0);
	}
}

@implementation RGEncoder

- (OSStatus)setupWidth:(int)w height:(int)h fps:(int)fps bitrate:(int)bitrate {
	NSDictionary *srcAttrs = @{
		(__bridge NSString *)kCVPixelBufferPixelFormatTypeKey : @(kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange),
		(__bridge NSString *)kCVPixelBufferIOSurfacePropertiesKey : @{},
	};
	// Prefer the low-latency hardware rate controller (Apple Silicon / macOS 11.3+),
	// fall back to plain hardware, then let VT choose.
	NSArray *specs = @[
		@{ (__bridge NSString *)kVTVideoEncoderSpecification_EnableHardwareAcceleratedVideoEncoder : @YES,
		   (__bridge NSString *)kVTVideoEncoderSpecification_EnableLowLatencyRateControl : @YES },
		@{ (__bridge NSString *)kVTVideoEncoderSpecification_EnableHardwareAcceleratedVideoEncoder : @YES },
		@{},
	];
	OSStatus s = -1;
	for (NSDictionary *spec in specs) {
		s = VTCompressionSessionCreate(NULL, w, h, kCMVideoCodecType_H264, (__bridge CFDictionaryRef)spec,
		                               (__bridge CFDictionaryRef)srcAttrs, NULL, vtOutput, (__bridge void *)self, &vt);
		if (s == noErr) break;
	}
	if (s != noErr) return s;

	VTSessionSetProperty(vt, kVTCompressionPropertyKey_RealTime, kCFBooleanTrue);
	if (VTSessionSetProperty(vt, kVTCompressionPropertyKey_ProfileLevel,
	                         kVTProfileLevel_H264_ConstrainedHigh_AutoLevel) != noErr)
		VTSessionSetProperty(vt, kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_Main_AutoLevel);
	VTSessionSetProperty(vt, kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse); // no B-frames
	VTSessionSetProperty(vt, kVTCompressionPropertyKey_AverageBitRate, (__bridge CFNumberRef)@(bitrate));
	VTSessionSetProperty(vt, kVTCompressionPropertyKey_ExpectedFrameRate, (__bridge CFNumberRef)@(fps));
	VTSessionSetProperty(vt, kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration, (__bridge CFNumberRef)@(10));
	// cap bursts at 1.5x average over one second
	NSArray *limits = @[ @(bitrate * 3 / 2 / 8), @1 ];
	VTSessionSetProperty(vt, kVTCompressionPropertyKey_DataRateLimits, (__bridge CFArrayRef)limits);
	VTCompressionSessionPrepareToEncodeFrames(vt);

	CFBooleanRef hw = NULL;
	if (VTSessionCopyProperty(vt, kVTCompressionPropertyKey_UsingHardwareAcceleratedVideoEncoder, NULL, &hw) == noErr && hw) {
		NSLog(@"VideoToolbox H.264 %dx%d@%d %d kbps, hardware=%s", w, h, fps, bitrate / 1000,
		      CFBooleanGetValue(hw) ? "yes" : "no");
		CFRelease(hw);
	}
	forceKey = YES;
	lastPTS = kCMTimeInvalid;
	return noErr;
}

// Must be called on q (or single-threaded in selftest).
- (void)encode:(CVPixelBufferRef)pb pts:(CMTime)pts {
	if (stopped || !vt) return;
	if (CMTIME_IS_VALID(lastPTS) && CMTimeCompare(pts, lastPTS) <= 0) pts = CMTimeAdd(lastPTS, CMTimeMake(1, 1000));
	lastPTS = pts;
	NSDictionary *opts = nil;
	if (forceKey) {
		opts = @{ (__bridge NSString *)kVTEncodeFrameOptionKey_ForceKeyFrame : @YES };
		forceKey = NO;
	}
	VTCompressionSessionEncodeFrame(vt, pb, pts, kCMTimeInvalid, (__bridge CFDictionaryRef)opts, NULL, NULL);
}

- (void)stream:(SCStream *)s didOutputSampleBuffer:(CMSampleBufferRef)sb ofType:(SCStreamOutputType)type {
	if (type != SCStreamOutputTypeScreen || stopped) return;
	CFArrayRef atts = CMSampleBufferGetSampleAttachmentsArray(sb, false);
	if (!atts || CFArrayGetCount(atts) == 0) return;
	CFDictionaryRef d = CFArrayGetValueAtIndex(atts, 0);
	CFNumberRef st = CFDictionaryGetValue(d, (__bridge CFStringRef)SCStreamFrameInfoStatus);
	int status = -1;
	if (!st || !CFNumberGetValue(st, kCFNumberIntType, &status) || status != SCFrameStatusComplete) return;
	CVPixelBufferRef pb = CMSampleBufferGetImageBuffer(sb);
	if (!pb) return;
	if (last) CVPixelBufferRelease(last);
	last = CVPixelBufferRetain(pb);
	[self encode:pb pts:CMSampleBufferGetPresentationTimeStamp(sb)];
}

- (void)stream:(SCStream *)s didStopWithError:(NSError *)error {
	if (stopped) return;
	goOnStop(handle, (char *)(error.localizedDescription.UTF8String ?: "capture stopped"));
}

- (void)teardown {
	stopped = YES;
	if (vt) {
		VTCompressionSessionCompleteFrames(vt, kCMTimeInvalid);
		VTCompressionSessionInvalidate(vt);
		CFRelease(vt);
		vt = NULL;
	}
	if (last) {
		CVPixelBufferRelease(last);
		last = NULL;
	}
}
@end

void *rg_enc_start(int displayIndex, int maxWidth, int fps, int bitrate, uintptr_t handle,
                   int *outW, int *outH, char *err, int errlen) {
	@autoreleasepool {
		if (@available(macOS 12.3, *)) {
		} else {
			seterr(err, errlen, @"ScreenCaptureKit 需要 macOS 12.3+");
			return NULL;
		}
		CGDirectDisplayID ids[32];
		uint32_t n = 0;
		CGGetActiveDisplayList(32, ids, &n);
		if (displayIndex < 0 || (uint32_t)displayIndex >= n) {
			seterr(err, errlen, @"显示器不存在");
			return NULL;
		}
		CGDirectDisplayID did = ids[displayIndex];

		__block SCShareableContent *content = nil;
		__block NSError *cerr = nil;
		dispatch_semaphore_t sem = dispatch_semaphore_create(0);
		[SCShareableContent getShareableContentWithCompletionHandler:^(SCShareableContent *c, NSError *e) {
			content = c;
			cerr = e;
			dispatch_semaphore_signal(sem);
		}];
		if (dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC)) != 0 || !content) {
			seterr(err, errlen, cerr ? cerr.localizedDescription : @"获取可共享内容超时（检查「屏幕录制」权限）");
			return NULL;
		}
		SCDisplay *disp = nil;
		for (SCDisplay *d in content.displays)
			if (d.displayID == did) disp = d;
		if (!disp) {
			seterr(err, errlen, @"ScreenCaptureKit 找不到该显示器");
			return NULL;
		}

		// Native pixel size (Retina aware), scaled down to maxWidth, capped at
		// 4096 to stay within H.264 level 5.1/5.2, even dimensions for NV12.
		size_t pw = disp.width, ph = disp.height;
		CGDisplayModeRef mode = CGDisplayCopyDisplayMode(did);
		if (mode) {
			pw = CGDisplayModeGetPixelWidth(mode);
			ph = CGDisplayModeGetPixelHeight(mode);
			CGDisplayModeRelease(mode);
		}
		int limit = maxWidth > 0 ? maxWidth : 1920;
		if (limit > 4096) limit = 4096;
		int w = (int)pw, h = (int)ph;
		if (w > limit) {
			h = (int)((double)h * limit / w);
			w = limit;
		}
		w &= ~1;
		h &= ~1;

		RGEncoder *e = [RGEncoder new];
		e->handle = handle;
		e->q = dispatch_queue_create("remote-go.capture", DISPATCH_QUEUE_SERIAL);
		OSStatus s = [e setupWidth:w height:h fps:fps bitrate:bitrate];
		if (s != noErr) {
			seterr(err, errlen, [NSString stringWithFormat:@"VTCompressionSessionCreate 失败: %d", (int)s]);
			return NULL;
		}

		SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:disp excludingWindows:@[]];
		SCStreamConfiguration *cfg = [SCStreamConfiguration new];
		cfg.width = w;
		cfg.height = h;
		cfg.minimumFrameInterval = CMTimeMake(1, fps);
		cfg.pixelFormat = kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange;
		cfg.queueDepth = 5;
		cfg.showsCursor = YES;
		cfg.scalesToFit = YES;

		e->stream = [[SCStream alloc] initWithFilter:filter configuration:cfg delegate:e];
		NSError *aerr = nil;
		if (![e->stream addStreamOutput:e type:SCStreamOutputTypeScreen sampleHandlerQueue:e->q error:&aerr]) {
			[e teardown];
			seterr(err, errlen, aerr.localizedDescription);
			return NULL;
		}
		__block NSError *serr = nil;
		[e->stream startCaptureWithCompletionHandler:^(NSError *se) {
			serr = se;
			dispatch_semaphore_signal(sem);
		}];
		if (dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC)) != 0 || serr) {
			[e teardown];
			seterr(err, errlen, serr ? serr.localizedDescription : @"启动采集超时");
			return NULL;
		}
		*outW = w;
		*outH = h;
		return (__bridge_retained void *)e;
	}
}

void rg_enc_keyframe(void *p) {
	RGEncoder *e = (__bridge RGEncoder *)p;
	dispatch_async(e->q, ^{
		if (e->stopped) return;
		e->forceKey = YES;
		// The screen may be static (SCK sends no new frames), so re-encode the
		// last frame to produce the keyframe immediately.
		if (e->last) [e encode:e->last pts:CMClockGetTime(CMClockGetHostTimeClock())];
	});
}

void rg_enc_stop(void *p) {
	@autoreleasepool {
		RGEncoder *e = (__bridge_transfer RGEncoder *)p;
		e->stopped = YES;
		dispatch_semaphore_t sem = dispatch_semaphore_create(0);
		[e->stream stopCaptureWithCompletionHandler:^(NSError *se) {
			dispatch_semaphore_signal(sem);
		}];
		dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 3 * NSEC_PER_SEC));
		dispatch_sync(e->q, ^{
			[e teardown];
		});
	}
}

int rg_enc_selftest(int width, int height, int frames, uintptr_t handle, char *err, int errlen) {
	@autoreleasepool {
		RGEncoder *e = [RGEncoder new];
		e->handle = handle;
		OSStatus s = [e setupWidth:width height:height fps:30 bitrate:4000000];
		if (s != noErr) {
			seterr(err, errlen, [NSString stringWithFormat:@"VTCompressionSessionCreate 失败: %d", (int)s]);
			return -1;
		}
		NSDictionary *attrs = @{ (__bridge NSString *)kCVPixelBufferIOSurfacePropertiesKey : @{} };
		for (int i = 0; i < frames; i++) {
			CVPixelBufferRef pb = NULL;
			if (CVPixelBufferCreate(NULL, width, height, kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
			                        (__bridge CFDictionaryRef)attrs, &pb) != kCVReturnSuccess) {
				seterr(err, errlen, @"CVPixelBufferCreate 失败");
				[e teardown];
				return -1;
			}
			CVPixelBufferLockBaseAddress(pb, 0);
			for (int plane = 0; plane < 2; plane++) {
				uint8_t *base = CVPixelBufferGetBaseAddressOfPlane(pb, plane);
				size_t stride = CVPixelBufferGetBytesPerRowOfPlane(pb, plane);
				size_t rows = CVPixelBufferGetHeightOfPlane(pb, plane);
				for (size_t y = 0; y < rows; y++)
					for (size_t x = 0; x < stride; x++)
						base[y * stride + x] = plane == 0 ? (uint8_t)(x + y + i * 4) : 128;
			}
			CVPixelBufferUnlockBaseAddress(pb, 0);
			[e encode:pb pts:CMTimeMake(i, 30)];
			CVPixelBufferRelease(pb);
		}
		VTCompressionSessionCompleteFrames(e->vt, kCMTimeInvalid);
		[e teardown];
		return 0;
	}
}
