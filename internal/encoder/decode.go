//go:build !linux || android

package encoder

// CanDecodeH264 reports whether this desktop's screen view can decode
// H.264, so a controller asks the host for it; otherwise it takes JPEG.
// macOS and Windows decode with the system; Android plays in a WebView.
func CanDecodeH264() bool { return true }
