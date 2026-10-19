// The screen's brightness as go-dock sets it (docs/protocol-v1.md §3.3),
// kept in NVS so it holds from power-on, before go-dock connects.
#pragma once

#include <stdbool.h>
#include <stdint.h>

typedef struct {
    uint8_t brightness;     // percent, 10-100
    uint8_t dim_brightness; // percent, 0-100; 0 turns the screen off
    uint32_t dim_after;     // seconds without an active task; 0 never dims
} settings_t;

// Loads the saved settings, else the defaults. Call it before the tasks
// start.
void settings_init(void);

// The settings now; from any task.
settings_t settings_get(void);

// Takes new settings, already in range, and saves them when they changed;
// returns whether they did.
bool settings_set(settings_t s);
