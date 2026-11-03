// Links to go-dock (docs/protocol-v1.md): newline-delimited JSON over TCP
// on WiFi, which carries what the screen shows, and over the native
// USB-Serial/JTAG port, which only sets up the WiFi and updates the
// firmware. One go-dock at a time has the screen; what the screen sends
// goes to it.
#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "model.h"

#define DOCK_PROTO 1

// Installs the USB driver and starts the USB and TCP tasks; the TCP one
// waits for wlan_start.
void link_start(void);

// Copies the newest snapshot into out when it is newer than *version.
// received_us is the esp_timer time it arrived.
bool link_take_snapshot(dock_snapshot_t *out, uint32_t *version, int64_t *received_us);

// Copies the newest line of readings into out when it is newer than
// *version.
bool link_take_line(dock_line_t *out, uint32_t *version);

// The name of the computer whose go-dock the screen shows ("" none yet),
// as its hello says.
void link_host_name(char *out, size_t n);

// esp_timer time of the last protocol message from the host, 0 if none yet.
int64_t link_last_rx_us(void);

// True when the host speaks another protocol major version.
bool link_incompatible(void);

// Sends a log line to the host, which shows it in its web console.
void link_log(const char *level, const char *fmt, ...) __attribute__((format(printf, 2, 3)));
