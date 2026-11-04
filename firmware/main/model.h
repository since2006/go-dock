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
    char title[104];   // session name, shown after the project (optional)
    char msg[152];    // first line of the assistant's latest text (optional)
    char detail[152]; // what the session is doing now (optional)
    int64_t since;    // unix seconds
    int64_t turn_output; // output tokens of the current turn (optional)
    int64_t turn_secs;   // work time of the current turn as of snapshot.now (optional)
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

// The line of readings above the usage (docs/protocol-v1.md §3.5, "line").
// Text buffers hold the host's limits in runes at 4 bytes each.
#define DOCK_MAX_READINGS 8
#define DOCK_ID_LEN 17 // [a-z0-9_-]{1,16}

typedef enum {
    DOCK_ICON_NONE = 0, // also for icons this firmware does not know
    DOCK_ICON_CPU,
    DOCK_ICON_GPU,
    DOCK_ICON_MEMORY,
    DOCK_ICON_ROTOR, // a fan's blades
    DOCK_ICON_TEMP, // a thermometer
    DOCK_ICON_DISK,
    DOCK_ICON_COUNT,
} dock_icon_t;

typedef enum {
    DOCK_LEVEL_OK = 0,
    DOCK_LEVEL_WARN, // amber
    DOCK_LEVEL_CRIT, // red
} dock_level_t;

typedef struct {
    char id[DOCK_ID_LEN];
    dock_icon_t icon;
    char label[20]; // <= 4 runes
    char text[36];  // <= 8 runes, the reading
    char unit[20];  // <= 4 runes, after it
    char sub[36];   // <= 8 runes, instead of the bar
    int8_t pct;     // 0-100, the bar; -1 none
    dock_level_t level;
} dock_reading_t;

typedef struct {
    int count;
    dock_reading_t items[DOCK_MAX_READINGS];
} dock_line_t;
