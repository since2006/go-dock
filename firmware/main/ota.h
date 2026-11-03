// Firmware update over the link (docs/protocol-v1.md §3.4): go-dock sends
// the app image in pieces, they go to the spare OTA partition, and the
// device restarts into it. Over the USB cable it starts at once; over WiFi
// the screen asks the user first. A new image is on probation until it
// reaches go-dock; if it restarts or stays silent first, the bootloader
// goes back to the previous one.
#pragma once

#include <stdbool.h>
#include <stddef.h>

#define OTA_CHUNK 2048 // largest piece of image in one ota_data message

typedef enum {
    OTA_IDLE = 0,
    OTA_ASKING,    // over WiFi: waiting for the user to say yes on the screen
    OTA_PREPARING, // erasing the spare partition
    OTA_RECEIVING,
    OTA_RESTARTING,
    OTA_FAILED,
} ota_state_t;

// Starts the probation of a freshly updated image. Call once at boot.
void ota_init(void);

// The image talked to go-dock: it stays.
void ota_confirm(void);

// Steps of an update, called from the link's reader tasks one at a time.
// Each returns NULL on success or what went wrong; any failure drops the
// update.
const char *ota_begin(size_t size, const char *version); // USB: erases now
const char *ota_write(size_t off, const char *base64, size_t *written);
const char *ota_finish(void); // checks the image and makes it the boot image
// Drops the update, or the question before it, saying why.
void ota_abort(const char *why);

// Over WiFi: ota_ask puts the update of size bytes to version, from the
// computer named from, to the user; the screen answers with ota_answer.
// ota_asked tells the link what they said, once: 1 yes (then call
// ota_begin_asked, which erases), -1 no, -2 nobody answered in time, 0 not
// yet.
const char *ota_ask(size_t size, const char *version, const char *from);
void ota_answer(bool yes);
int ota_asked(void);
const char *ota_begin_asked(void);

// True from the question to the last piece: another link may not start one.
bool ota_busy(void);

// Drops an update the host stopped feeding. Call regularly.
void ota_tick(void);

// For the screen: the update's state, its progress (0-100) and, when it
// failed, why. Once the user answered, the question shows as the update
// preparing, or as gone.
ota_state_t ota_status(int *percent, const char **error);

// For the question: who asks and how many seconds it stays up; and whether
// the update comes over WiFi (the screen must keep its power, not its
// cable).
const char *ota_from(void);
int ota_ask_left(void);
bool ota_over_wifi(void);

// Version of the running firmware, and of the image being received.
const char *ota_version(void);
const char *ota_target(void);
