// USB link to go-dock: newline-delimited JSON over the native
// USB-Serial/JTAG port (docs/protocol-v1.md).
#pragma once

#include <stdbool.h>
#include <stdint.h>

#include "model.h"

#define DOCK_PROTO 1

// Installs the USB driver and starts the reader task.
void link_start(void);

// Copies the newest snapshot into out when it is newer than *version.
// received_us is the esp_timer time it arrived.
bool link_take_snapshot(dock_snapshot_t *out, uint32_t *version, int64_t *received_us);

// esp_timer time of the last protocol message from the host, 0 if none yet.
int64_t link_last_rx_us(void);

// True when the host speaks another protocol major version.
bool link_incompatible(void);

// Sends a log line to the host, which shows it in its web console.
void link_log(const char *level, const char *fmt, ...) __attribute__((format(printf, 2, 3)));
