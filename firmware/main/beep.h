// Alert sounds on a passive buzzer wired to the pin header (J8 pin 3 =
// GPIO1, pin 1 = GND). The board has no speaker of its own; go-dock sends
// the notes to play in a beep message (docs/protocol-v1.md §3.6).
#pragma once

#include <stdint.h>

// A beep line of this many notes, the longest being [8000,1000], still fits
// MAX_LINE in link.c (go-dock's TestBeepFitsLine).
#define BEEP_MAX_NOTES 320

typedef struct {
    uint16_t hz; // 0: rest
    uint16_t ms;
} beep_note_t;

// Sets up the PWM output and starts the player task. Call once at boot.
void beep_init(void);

// Plays notes in the background, cutting off whatever is playing.
void beep_play(const beep_note_t *notes, int count);
