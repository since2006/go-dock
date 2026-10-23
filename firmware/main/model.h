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
    char title[104];  // session name, shown after the project (optional)
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

// Function pages next to the Agent page (docs/protocol-v1.md, "pages").
// Text buffers hold the host's limits in runes at 4 bytes each.
#define DOCK_MAX_PAGES 4
#define DOCK_MAX_ITEMS 6 // per page
#define DOCK_ID_LEN 17   // [a-z0-9_-]{1,16}

typedef enum {
    DOCK_ITEM_OTHER = 0, // a kind this firmware does not know: shown, not tappable
    DOCK_ITEM_SWITCH,    // on / off: a tap toggles
    DOCK_ITEM_STEPPER,   // a value with − and +: dec, inc
    DOCK_ITEM_SELECT,    // one of a few options: a tap takes the next
    DOCK_ITEM_METER,     // a reading with a chart or a bar: no taps
} dock_item_kind_t;

typedef enum {
    DOCK_ICON_BULB = 0, // also for icons this firmware does not know
    DOCK_ICON_POWER,
    DOCK_ICON_COOL,
    DOCK_ICON_HEAT,
    DOCK_ICON_DRY,
    DOCK_ICON_FAN,
    DOCK_ICON_AUTO,
    DOCK_ICON_MOON,
    DOCK_ICON_BELL,
    DOCK_ICON_CPU, // the 性能 page
    DOCK_ICON_GPU,
    DOCK_ICON_MEMORY,
    DOCK_ICON_ROTOR, // a fan's blades
    DOCK_ICON_NET,
    DOCK_ICON_DISK,
    DOCK_ICON_COUNT,
} dock_icon_t;

// A meter's readings (docs/protocol-v1.md §3.5).
#define DOCK_MAX_HIST 30
#define DOCK_MAX_SERIES 3
#define DOCK_MAX_ROWS 4

typedef enum {
    DOCK_COLOR_NONE = 0, // the card's own colour
    DOCK_COLOR_BLUE,
    DOCK_COLOR_RED,
    DOCK_COLOR_TEAL,
    DOCK_COLOR_GREEN,
    DOCK_COLOR_ORANGE,
    DOCK_COLOR_PURPLE,
    DOCK_COLOR_GRAY,
    DOCK_COLOR_COUNT,
} dock_color_t;

typedef enum {
    DOCK_LEVEL_OK = 0,
    DOCK_LEVEL_WARN, // amber
    DOCK_LEVEL_CRIT, // red
} dock_level_t;

typedef struct {
    uint8_t n;
    uint8_t v[DOCK_MAX_HIST]; // 0-100, oldest first
} dock_hist_t;

typedef struct {
    dock_color_t color;
    dock_hist_t hist;
} dock_series_t;

typedef struct {
    char label[28]; // <= 6 runes
    char text[36];  // <= 8 runes
    dock_color_t color;
} dock_row_t;

typedef struct {
    char id[DOCK_ID_LEN];
    dock_item_kind_t kind;
    dock_icon_t icon;
    char label[52]; // <= 12 runes
    char text[36];  // <= 8 runes: 已开, 已关, 切换中…, 26°C, 制冷
    int8_t on;      // 1 on, 0 off, -1 unknown
    bool buttons;   // a switch with an on and an off button instead of a toggle
    bool busy;      // a command is on its way
    bool disabled;  // cannot be used now (MQTT down): greyed, no taps
    bool at_min;    // a stepper's value cannot go lower
    bool at_max;    // or higher
    // A meter: text with unit after it, sub a short note, pct (0-100, -1
    // none) a bar or hist a line; a big one also note, series and rows.
    char unit[20];  // <= 4 runes
    char sub[36];   // <= 8 runes
    char note[68];  // <= 16 runes
    int8_t pct;
    dock_level_t level;
    bool big;       // in a dashboard's top row
    dock_hist_t hist;
    uint8_t series_count;
    dock_series_t series[DOCK_MAX_SERIES]; // the first in front
    uint8_t row_count;
    dock_row_t rows[DOCK_MAX_ROWS];
} dock_item_t;

typedef struct {
    char id[DOCK_ID_LEN];
    char title[36];    // <= 8 runes
    char summary[100]; // <= 24 runes
    char error[100];   // <= 24 runes; shown instead of summary
    bool movable;      // a card held down can be dragged onto another to swap
    int item_count;
    dock_item_t items[DOCK_MAX_ITEMS];
} dock_page_t;

typedef struct {
    int count;
    dock_page_t pages[DOCK_MAX_PAGES];
} dock_pages_t;
