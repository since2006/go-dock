// The screen: the Agent page and the function pages to its right
// (docs/protocol-v1.md §7.1).
#pragma once

#include <stdint.h>

// Builds the screen on the active LVGL display.
void ui_create(void);

// Applies new data from the link and refreshes clocks and timers. Call it
// from the LVGL task, often (it is cheap when nothing changed).
void ui_poll(void);

// Brightness the UI wants right now, 0-255. Call it before each
// lv_timer_handler: a tap on a screen it left dim only wakes it up.
uint8_t ui_brightness(void);

// Called by the touch driver on every press; wakes a dimmed screen.
void ui_touched(void);
