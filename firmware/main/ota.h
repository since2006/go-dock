// Firmware update over the link (docs/protocol-v1.md §3.4): go-dock sends
// the app image in pieces, they go to the spare OTA partition, and the
// device restarts into it. A new image is on probation until it reaches
// go-dock; if it restarts or stays silent first, the bootloader goes back
// to the previous one.
#pragma once

#include <stdbool.h>
#include <stddef.h>

#define OTA_CHUNK 2048 // largest piece of image in one ota_data message

typedef enum {
    OTA_IDLE = 0,
    OTA_PREPARING, // erasing the spare partition
    OTA_RECEIVING,
    OTA_RESTARTING,
    OTA_FAILED,
} ota_state_t;

// Starts the probation of a freshly updated image. Call once at boot.
void ota_init(void);

// True when there is a partition to update into.
bool ota_supported(void);

// The image talked to go-dock: it stays.
void ota_confirm(void);

// Steps of an update, called from the link's reader task. Each returns
// NULL on success or what went wrong; any failure drops the update.
const char *ota_begin(size_t size, const char *version);
const char *ota_write(size_t off, const char *base64, size_t *written);
const char *ota_finish(void); // checks the image and makes it the boot image
void ota_abort(void);

// Drops an update the host stopped feeding. Call regularly.
void ota_tick(void);

// For the screen: the update's state, its progress (0-100) and, when it
// failed, why.
ota_state_t ota_status(int *percent, const char **error);

// Version of the running firmware, and of the image being received.
const char *ota_version(void);
const char *ota_target(void);
