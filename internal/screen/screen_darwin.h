#include <stdint.h>

void *rg_screen_new(uintptr_t h);
void rg_screen_free(void *p);
int rg_screen_h264(void *p, const uint8_t *d, int n, int key);
void rg_screen_jpeg(void *p, const uint8_t *d, int n);
void rg_screen_reset(void *p);
void rg_screen_focus(void *p);
