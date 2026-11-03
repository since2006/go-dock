// WiFi station (docs/protocol-v1.md §1.1): joins the network go-dock set up
// over USB and keeps that setting and the pairing code in NVS.
#pragma once

#include <stdbool.h>
#include <stdint.h>

#define WLAN_PORT 47322 // the TCP port link.c listens on

typedef enum {
    WLAN_OFF = 0,    // not set up
    WLAN_CONNECTING, // joining, or lost the network and joining again
    WLAN_CONNECTED,
    WLAN_FAILED,     // could not join; err says why, and it keeps trying
} wlan_state_t;

typedef struct {
    wlan_state_t state;
    char ssid[33];
    char ip[16];  // dotted quad once connected
    bool fixed;   // a fixed address rather than DHCP
    char err[64]; // why joining failed
} wlan_status_t;

typedef struct {
    char ssid[33]; // empty: WiFi off
    char pass[64]; // empty: an open network
    char ip[16];   // empty: DHCP
    char mask[16];
    char gw[16];
} wlan_config_t;

#define WLAN_MAX_APS 20 // networks a scan reports

typedef struct {
    char ssid[33];
    int8_t rssi; // dBm
    bool open;   // no password
} wlan_ap_t;

typedef struct {
    int count;
    wlan_ap_t aps[WLAN_MAX_APS]; // strongest first, one per name
    char err[64];                // why the scan failed
} wlan_scan_t;

// Brings up the network stack and joins the saved network, if any. Call it
// once, after the display took its internal memory.
void wlan_start(void);

// True once wlan_start has brought up the network stack (sockets work) and
// loaded the pairing code.
bool wlan_ready(void);

// Saves c and joins it (an empty ssid turns WiFi off). Returns why c was
// refused, or NULL. From the link task only.
const char *wlan_configure(const wlan_config_t *c);

// The WiFi state now; from any task.
wlan_status_t wlan_status(void);

// Bumps on every change of wlan_status, so the link can report it.
uint32_t wlan_version(void);

// The six-digit pairing code a network link must say hello with.
const char *wlan_code(void);

// Starts scanning for networks in range, for go-dock to offer them; the
// result comes through wlan_take_scan in a few seconds. Returns why it
// cannot start, or NULL. From the link task only.
const char *wlan_scan(void);

// Copies the newest scan result into out when it is newer than *version.
bool wlan_take_scan(wlan_scan_t *out, uint32_t *version);
