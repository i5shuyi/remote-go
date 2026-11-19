package screen

/*
#include <stdlib.h>
void rg_snapshot(const char *dir);
*/
import "C"

import "unsafe"

// Snapshot writes each visible window of the app to dir as window-N.png.
func Snapshot(dir string) {
	c := C.CString(dir)
	defer C.free(unsafe.Pointer(c))
	C.rg_snapshot(c)
}
