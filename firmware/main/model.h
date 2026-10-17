// Display state received from go-dock (docs/protocol-v1.md, "snapshot").
#pragma once

#include <stdbool.h>
#include <stdint.h>

#define DOCK_MAX_SESSIONS 32

typedef enum {
    DOCK_APPROVAL = 0,
    DOCK_INPUT,
    DOCK_ERROR,
    DOCK_RUNNING,
    DOCK_DONE,
    DOCK_IDLE,
    DOCK_STATE_COUNT,
} dock_state_t;

typedef struct {
    bool present;
    float pct;
    int64_t resets_at; // unix seconds, 0 = unknown
} dock_window_t;

typedef struct {
    dock_window_t five_hour;
    dock_window_t seven_day;
    int64_t today_tokens;
} dock_usage_t;

typedef enum {
    DOCK_CLAUDE = 0,
    DOCK_CODEX,
} dock_provider_t;

typedef struct {
    char id[96];
    dock_provider_t provider;
    dock_state_t state;
    char project[104]; // <= 24 runes of UTF-8
    char label[32];
    char text[200]; // <= 48 runes of UTF-8
    int64_t since;  // unix seconds
    int agents_total;  // subagents (child tasks) of this session
    int agents_active; // of which still working
} dock_session_t;

typedef struct {
    uint32_t seq;
    int64_t now;       // host time when sent, unix seconds
    int32_t tz_offset; // host's UTC offset in seconds, for the clock
    dock_usage_t usage[2]; // indexed by dock_provider_t
    int count;
    dock_session_t sessions[DOCK_MAX_SESSIONS];
    int counts[DOCK_STATE_COUNT];
} dock_snapshot_t;
