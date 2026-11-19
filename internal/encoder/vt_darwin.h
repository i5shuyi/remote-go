#include <stdint.h>

// Starts ScreenCaptureKit capture of the display at displayIndex and encodes it
// with VideoToolbox H.264. Frames are delivered to goOnFrame(handle, ...).
// Returns NULL and fills err on failure. outW/outH receive the encoded size.
void *rg_enc_start(int displayIndex, int maxWidth, int fps, int bitrate, uintptr_t handle,
                   int *outW, int *outH, char *err, int errlen);
void rg_enc_keyframe(void *enc);
void rg_enc_stop(void *enc);

// Test helper: encodes `frames` synthetic NV12 frames without screen capture.
int rg_enc_selftest(int width, int height, int frames, uintptr_t handle, char *err, int errlen);
