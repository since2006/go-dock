// Display state received from go-dock (docs/protocol-v1.md, "snapshot").
#pragma once

#include <stdbool.h>
#include <stdint.h>

#define DOCK_MAX_SESSIONS 32
#define DOCK_MAX_AGENTS 3 // subagents listed per session

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
    char type[52]; // Explore, general-purpose, ...
    char text[104];
    dock_state_t state;
    int64_t since;
} dock_agent_t;

typedef struct {
    char id[96];
    dock_provider_t provider;
    dock_state_t state;
    char project[104]; // <= 24 runes of UTF-8
    char label[32];
    char text[200];   // one-line summary, <= 48 runes of UTF-8
    char title[104];  // what the task is (optional)
    char detail[152]; // what the session is doing now (optional)
    int64_t since;    // unix seconds
    int agents_total;  // subagents (child tasks) of this session
    int agents_active; // of which still working
    int agent_count;   // entries in agents
    dock_agent_t agents[DOCK_MAX_AGENTS];
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
