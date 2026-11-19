#import <AppKit/AppKit.h>

uint64_t rg_clip_version(void) {
	@autoreleasepool { return NSPasteboard.generalPasteboard.changeCount; }
}

int rg_clip_read(void *out, int max) {
	@autoreleasepool {
		NSString *text = [NSPasteboard.generalPasteboard stringForType:NSPasteboardTypeString];
		NSUInteger count = [text lengthOfBytesUsingEncoding:NSUTF8StringEncoding];
		if (!count || count > max) return 0;
		return [text getBytes:out maxLength:max usedLength:NULL encoding:NSUTF8StringEncoding options:0
		              range:NSMakeRange(0, text.length) remainingRange:NULL] ? (int)count : -1;
	}
}

int rg_clip_write(const void *data, int n) {
	@autoreleasepool {
		NSString *text = [[[NSString alloc] initWithBytes:data length:n encoding:NSUTF8StringEncoding] autorelease];
		if (!text) return 0;
		NSPasteboard *board = NSPasteboard.generalPasteboard;
		[board clearContents];
		return [board setString:text forType:NSPasteboardTypeString];
	}
}

int rg_clip_rich_read(void **out, int *size, int maxImage, int maxFiles, int maxPixels) {
	@autoreleasepool {
		NSPasteboard *board = NSPasteboard.generalPasteboard;
		NSArray *urls = [board readObjectsForClasses:@[[NSURL class]]
			options:@{ NSPasteboardURLReadingFileURLsOnlyKey: @YES }];
		NSData *data = nil;
		int kind = 1;
		if (urls.count) {
			if (urls.count > maxFiles) return -1;
			NSMutableArray *paths = [NSMutableArray array];
			for (NSURL *url in urls) {
				if (!url.isFileURL || !url.path || (url.host.length && ![url.host isEqualToString:@"localhost"])) return -1;
				[paths addObject:url.path];
			}
			data = [NSJSONSerialization dataWithJSONObject:paths options:0 error:NULL];
			kind = 2;
		} else {
			data = [board dataForType:NSPasteboardTypePNG];
			if (!data) {
				NSData *encoded = [board dataForType:NSPasteboardTypeTIFF];
				if (!encoded) encoded = [board dataForType:@"public.jpeg"];
				if (!encoded) return 0;
				if (encoded.length > (uint64_t)maxPixels*8) return -1;
				NSBitmapImageRep *rep = [NSBitmapImageRep imageRepWithData:encoded];
				if (!rep || rep.pixelsHigh <= 0 || rep.pixelsWide <= 0 || rep.pixelsWide > maxPixels/rep.pixelsHigh) return -1;
				data = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
			}
		}
		if (!data.length || data.length > maxImage) return -1;
		*out = malloc(data.length);
		if (!*out) return -1;
		memcpy(*out,data.bytes,data.length);
		*size = (int)data.length;
		return kind;
	}
}

int rg_clip_rich_write(const void *bytes, int n, int files) {
	@autoreleasepool {
		NSData *data = [NSData dataWithBytes:bytes length:n];
		NSPasteboard *board = NSPasteboard.generalPasteboard;
		if (files) {
			NSArray *paths = [NSJSONSerialization JSONObjectWithData:data options:0 error:NULL];
			NSMutableArray *urls = [NSMutableArray array];
			for (NSString *path in paths) [urls addObject:[NSURL fileURLWithPath:path isDirectory:NO]];
			if (!urls.count) return 0;
			[board clearContents];
			return [board writeObjects:urls];
		}
		NSBitmapImageRep *rep = [NSBitmapImageRep imageRepWithData:data];
		if (!rep) return 0;
		NSPasteboardItem *item = [[[NSPasteboardItem alloc] init] autorelease];
		if (![item setData:data forType:NSPasteboardTypePNG]) return 0;
		NSData *tiff = rep.TIFFRepresentation;
		if (tiff) [item setData:tiff forType:NSPasteboardTypeTIFF];
		[board clearContents];
		return [board writeObjects:@[item]];
	}
}
