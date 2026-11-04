// Links to go-dock (docs/protocol-v1.md): newline-delimited JSON over TCP
// on WiFi, which carries what the screen shows, and over the native
// USB-Serial/JTAG port, which only sets up the WiFi and updates the
// firmware. Up to LINK_HOSTS go-docks share the screen, each with its own
// snapshot and line (a page each, see hosts.h); what the screen has to say
// goes to all of them.
#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "model.h"

#define DOCK_PROTO 1
#define LINK_HOSTS 4     // go-docks at once, one TCP link each
#define LINK_NAME_LEN 84 // a computer's name: 20 runes of UTF-8

// A go-dock on the screen, as link_hosts reports it. The summary comes
// from its newest snapshot (hosts.c host_note).
typedef struct {
    uint32_t gen; // 0: nobody in this slot; another number for each go-dock in it
    char id[17];  // from its hello: the same go-dock on a new link has the same id
    char name[LINK_NAME_LEN];
    bool incompatible;  // speaks another protocol major version: no page
    uint32_t version;   // of its newest snapshot, 0 none yet
    int64_t last_rx_us; // esp_timer time of its last message
    int64_t news_us;    // its last news worth showing (hosts.h), 0 none
    int64_t urgent_us;  // its last news waiting for the user, 0 none
    int count;          // tasks
    int waiting;        // of which waiting for the user (approval, input)
    int attention;      // of which needing the user (those and errors)
    bool active;        // a task works or waits
    dock_state_t top;   // the most urgent task's state, DOCK_STATE_COUNT none
} link_host_t;

// Installs the USB driver and starts the USB and TCP tasks; the TCP one
// waits for wlan_start.
void link_start(void);

// Copies the go-docks on the screen, by slot, into out.
void link_hosts(link_host_t out[LINK_HOSTS]);

// Copies the newest snapshot of the go-dock in slot (as long as it is
// still gen) into out when it is newer than *version. received_us is the
// esp_timer time it arrived.
bool link_take_snapshot(int slot, uint32_t gen, dock_snapshot_t *out, uint32_t *version, int64_t *received_us);

// The same for its line of readings; a go-dock that sent none has an empty
// one.
bool link_take_line(int slot, uint32_t gen, dock_line_t *out, uint32_t *version);

// Sends a log line to every go-dock on the screen, which shows it in its
// web console.
void link_log(const char *level, const char *fmt, ...) __attribute__((format(printf, 2, 3)));
