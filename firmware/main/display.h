// AMOLED panel, touch and the LVGL display/input setup.
#pragma once

#include <stdint.h>

// Powers the panel, initializes LVGL with a 600x450 landscape display and
// registers the touch input. Call from the LVGL task.
void display_init(void);

// Panel brightness, 0-255.
void display_set_brightness(uint8_t level);
