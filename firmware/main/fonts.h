// Fonts rendered at runtime from the TTF files embedded in the app
// (see tools/make_fonts.py).
#pragma once

#include "lvgl.h"

typedef enum {
    FONT_REGULAR,   // Noto Sans SC 400
    FONT_BOLD,      // Noto Sans SC 700
    FONT_MONO,      // JetBrains Mono 500, digits and ASCII
    FONT_MONO_BOLD, // JetBrains Mono 700
} font_face_t;

// Returns the font for face at size px, creating it on first use.
const lv_font_t *font_get(font_face_t face, int size);
