// Infrared through modules wired to the pin header: a 38 kHz transmitter
// on J8 pin 13 (GPIO7) and a receiver on pin 15 (GPIO8). go-dock encodes
// the frames (docs/protocol-v1.md §3.7, §4.6); this only sends and
// receives raw timings in µs, mark and space in turn, from a mark.
#pragma once

#include <stdbool.h>
#include <stdint.h>

#define IR_MAX_RAW 512   // timings in a frame to send
#define IR_MAX_SPAN 32767 // µs, the longest timing RMT holds

// Sets up the transmitter and receiver and starts their tasks. Call once
// at boot, before link_start.
void ir_init(void);

// True when the transmitter is set up, so the device can say "ir" in its
// hello.
bool ir_ready(void);

// Queues a frame to send at hz. False when four are waiting already.
bool ir_send(uint32_t hz, const uint16_t *raw, int count);
