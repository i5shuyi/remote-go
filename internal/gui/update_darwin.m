//go:build darwin && cgo

#import <Foundation/Foundation.h>
#include <dlfcn.h>
#include <stdatomic.h>

static atomic_bool restarting;
void rg_watch_update_restart(void) {
    // Resolve the exported notification from the bundled copy of Sparkle.
    NSString *path = [[[NSBundle mainBundle] privateFrameworksPath] stringByAppendingPathComponent:@"Sparkle.framework/Sparkle"];
    void *framework = dlopen(path.fileSystemRepresentation, RTLD_LAZY | RTLD_LOCAL);
    if (!framework) return;
    NSString *const *name = dlsym(framework, "SUUpdaterWillRestartNotification");
    if (!name) return;
    [[NSNotificationCenter defaultCenter] addObserverForName:*name object:nil queue:nil
        usingBlock:^(NSNotification *notification) { atomic_store(&restarting, true); }];
}
int rg_update_restarting(void) { return atomic_load(&restarting); }
