// Objective-C side of setDockIcon (see dockicon_darwin.go). Kept in a
// .m file because cgo compiles a .go file's preamble as plain C; only
// .m files in the package are run through the Objective-C compiler.

#import <Cocoa/Cocoa.h>

void wkpSetDockIcon(const void *data, int len) {
    @autoreleasepool {
        NSData *d = [NSData dataWithBytes:data length:(NSUInteger)len];
        NSImage *img = [[NSImage alloc] initWithData:d];
        if (img != nil) {
            [NSApplication.sharedApplication setApplicationIconImage:img];
        }
    }
}
