#include "ui.h"

#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <time.h>

#include "esp_heap_caps.h"
#include "esp_timer.h"
#include "fonts.h"
#include "hosts.h"
#include "link.h"
#include "lvgl.h"
#include "model.h"
#include "ota.h"
#include "sdkconfig.h"
#include "settings.h"
#include "wlan.h"

#define SCR_W 600
#define SCR_H 450
#define PAD_X 18
#define TOP 14
#define TASKS_Y 178
#define TASKS_H 258
#define LIST_W 540
#define MAX_DOTS 8

#define OFFLINE_AFTER_US (15 * 1000000LL)
#define TOUCH_WAKE_US (60 * 1000000LL)
#define TITLE_MAX_W 300 // a longer computer name ends in "..."
#define INFO_HOLD_US (120 * 1000000LL) // the connection page goes this long after the last touch
#define SHIFT_EVERY_US (60 * 1000000LL)    // pixel shift against burn-in

// Panel levels with go-dock away, never above the brightness set (settings.h).
#define BRIGHT_OFFLINE 40
#define BRIGHT_ASLEEP 12

#define C_TEXT 0xF2F2F0
#define C_TEXT2 0xA3A39E
#define C_TEXT3 0x8C8B86
#define C_TEXT4 0x75746F
#define C_TEXT5 0x6F6E69
#define C_CARD 0x0F0F10
#define C_TRACK 0x232325
#define C_AMBER 0xFFB020
#define C_RED 0xFF6B5E
#define C_GREEN 0x3DD68C

static const uint32_t PROVIDER_COLOR[2] = {0xE8906E, 0x93A9FF};
static const char *PROVIDER_NAME[2] = {"Claude", "Codex"};

static const struct {
    uint32_t color;
    uint32_t row_bg;
    bool ring;
    const char *icon; // NULL: spinner
    const char *label;
} STATE[DOCK_STATE_COUNT] = {
    [DOCK_APPROVAL] = {C_AMBER, 0x241806, true, LV_SYMBOL_WARNING, "需授权"},
    [DOCK_INPUT] = {0x5AA9FF, C_CARD, false, LV_SYMBOL_BELL, "等待输入"},
    [DOCK_ERROR] = {C_RED, 0x220C0A, true, LV_SYMBOL_CLOSE, "出错"},
    [DOCK_RUNNING] = {C_GREEN, C_CARD, false, NULL, "进行中"},
    [DOCK_DONE] = {0x9A9A94, C_CARD, false, LV_SYMBOL_OK, "已完成"},
    [DOCK_IDLE] = {0x9A9A94, C_CARD, false, LV_SYMBOL_BULLET, "空闲"},
};

// Task list layout, shared with the web preview (docs/protocol-v1.md
// §7.1): a session row is 126 px, so two fill a page, with three full-width
// lines: the project and the state; the session name and the turn's time
// and tokens; one line of text (see row_text), without which the first two
// are centred. Each subagent adds a 36 px row under it; a session and its
// subagents never split across pages.
#define MAIN_H 126
#define SUB_H 36
#define SUB_GAP 4
#define GROUP_GAP 6
#define SUB_X 52
#define SUB_TIME_W 72  // a subagent's time, right-aligned
#define SUB_LABEL_W 72 // and its state before it
#define MAX_MAIN 2     // session rows that fit on a page
#define MAX_SUB 6      // subagent rows that fit on a page
#define MAX_PAGES 32
#define CHIP 44
#define MID_X (12 + CHIP + 12)
#define END_X (LIST_W - 14)     // right edge of the row's text
#define LINE_W (END_X - MID_X) // lines under the project line

// The line of readings above the usage, shared with the web preview
// (docs/protocol-v1.md §7.1, scrLine): three at a time, up to three sharing
// the line evenly. More take turns in groups of three on fixed columns,
// each staying READ_DWELL_US, the next sliding in from the right over
// READ_SLIDE_US, the first again after the last. The turning follows the
// clock, never the data. Two group objects take turns: the one shown, and
// the next while it slides in. With the line the usage card tightens
// (layout_usage), so the tasks keep their place.
#define READ_X (PAD_X + 2)
#define READ_Y 48 // 4 px under the strip and over the usage card
#define READ_W (SCR_W - 2 * PAD_X - 4)
#define READ_H 26
#define READ_N 3     // readings shown at a time
#define READ_GAP 12  // between readings, and between two groups sliding
#define READ_BASE 21 // text baseline in the line
#define READ_ICON 20
#define READ_BAR 20 // a bar is at least this wide
#define READ_DWELL_US (6 * 1000000LL)
#define READ_SLIDE_US (500 * 1000LL)

typedef struct {
    lv_obj_t *reset, *window, *pct, *bar;
} meter_t;

typedef struct {
    lv_obj_t *obj, *chip, *icon, *spinner, *project, *name, *prov, *label, *time, *tok, *text;
} row_t;

typedef struct {
    lv_obj_t *obj, *icon, *spinner, *type, *text, *label, *time;
} sub_t;

// One reading of the line: icon, label, the reading and its unit, then a
// bar or a note.
typedef struct {
    lv_obj_t *obj, *icon, *label, *value, *unit, *bar, *sub;
} seg_t;

static struct {
    lv_obj_t *root;    // shifted a few px now and then against burn-in
    lv_obj_t *content; // dimmed when the host is away
    lv_obj_t *title, *summary, *link_dot, *clock;
    lv_obj_t *hosts, *hdots[LINK_HOSTS], *hinfo; // after the name: a dot per computer, a ring for the connection page
    lv_obj_t *agent;                             // the Agent page under the strip: readings, usage, tasks
    lv_obj_t *info, *info_addr, *info_code, *info_hint, *info_hosts, *info_wifi; // the connection page
    lv_obj_t *line, *lgroup[2]; // the readings, clipped to the line
    seg_t seg[2][READ_N];
    lv_obj_t *usage_card;
    struct {
        lv_obj_t *row;
        meter_t m[2];
        lv_obj_t *today, *today_label;
    } usage[2];
    row_t rows[MAX_MAIN];
    sub_t subs[MAX_SUB];
    lv_obj_t *pager, *dots[MAX_DOTS];
    lv_obj_t *empty;
    lv_obj_t *overlay, *ov_icon, *ov_title, *ov_sub, *ov_note, *ov_bar, *ov_ver;
    lv_obj_t *ov_no, *ov_yes; // the answers to an update over WiFi
} ui;

static dock_snapshot_t *s_snap;
static uint32_t s_version;
static int64_t s_rx_us;
static bool s_have;
static int s_page;
static int s_page_count = 1;
static int s_page_start[MAX_PAGES + 1]; // first session index of each page
static char s_first_id[96];             // first session on the current page
static int64_t s_last_second_us;
static int64_t s_touch_us;
static int64_t s_active_us;
static int64_t s_shift_us;
static int s_shift;

// Whose page is on screen (hosts.c picks it): the slot and the go-dock in
// it, of which the screen keeps the name and last snapshot once it is gone.
static link_host_t s_hosts[LINK_HOSTS];
static int s_shown = -1;
static uint32_t s_shown_gen;
static char s_shown_id[17];
static char s_shown_name[LINK_NAME_LEN];
static int64_t s_shown_at;
static int64_t s_shown_rx_us;         // when it was last heard from, 0 nobody yet
static int64_t s_seen_us[LINK_HOSTS]; // when each slot's go-dock was last on screen
static uint32_t s_seen_gen[LINK_HOSTS];
static int s_order[LINK_HOSTS]; // the slots with a page, by id: the dots, swiping
static int s_order_n;
static bool s_attention; // some page has tasks that need the user
static bool s_no_page;   // go-docks are linked, but none can show (another protocol)

// The connection page (docs/protocol-v1.md §7.1, «连接信息页»): where another
// computer reaches the screen. Only a swipe or a tap brings it up, never
// the turns; it goes INFO_HOLD_US after the last touch, or when a computer
// joins, showing that one's page. The computers' pages keep their place
// under it (s_shown).
static bool s_info;
static int64_t s_info_at;
static char s_info_ids[LINK_HOSTS][17]; // the computers on the screen when it came up

static dock_line_t s_line;
static uint32_t s_line_version;
static char s_line_ids[DOCK_MAX_READINGS * DOCK_ID_LEN]; // to start over when they change
static int64_t s_line_start_us;                          // the first group came up
static int s_lshown[2] = {-1, -1};                       // the group each group object shows
static bool s_lined;                                     // the line is up

// ---------- helpers ----------

static lv_obj_t *box(lv_obj_t *parent, int x, int y, int w, int h, uint32_t bg, int radius)
{
    lv_obj_t *o = lv_obj_create(parent);
    lv_obj_remove_style_all(o);
    lv_obj_remove_flag(o, LV_OBJ_FLAG_SCROLLABLE);
    lv_obj_set_pos(o, x, y);
    lv_obj_set_size(o, w, h);
    if (radius >= 0) {
        lv_obj_set_style_bg_color(o, lv_color_hex(bg), 0);
        lv_obj_set_style_bg_opa(o, LV_OPA_COVER, 0);
        lv_obj_set_style_radius(o, radius, 0);
    }
    return o;
}

static lv_obj_t *text(lv_obj_t *parent, const lv_font_t *font, uint32_t color)
{
    lv_obj_t *l = lv_label_create(parent);
    lv_obj_set_style_text_font(l, font, 0);
    lv_obj_set_style_text_color(l, lv_color_hex(color), 0);
    lv_label_set_text(l, "");
    return l;
}

// One-line label that ends in "..." when longer than w.
static lv_obj_t *line(lv_obj_t *parent, const lv_font_t *font, uint32_t color, int w)
{
    lv_obj_t *l = text(parent, font, color);
    lv_label_set_long_mode(l, LV_LABEL_LONG_DOT);
    lv_obj_set_size(l, w, lv_font_get_line_height(font));
    return l;
}

static void set_text(lv_obj_t *l, const char *s)
{
    if (strcmp(lv_label_get_text(l), s) != 0) {
        lv_label_set_text(l, s);
    }
}

static void set_color(lv_obj_t *l, uint32_t c)
{
    lv_obj_set_style_text_color(l, lv_color_hex(c), 0);
}

// Distance from a label's top to its text baseline.
static int ascent(lv_obj_t *l)
{
    const lv_font_t *f = lv_obj_get_style_text_font(l, 0);
    return lv_font_get_line_height(f) - f->base_line;
}

// Places a label at x with its baseline at y, so text of different sizes on
// one line sits on the same baseline.
static void on_baseline(lv_obj_t *l, int x, int y)
{
    lv_obj_set_pos(l, x, y - ascent(l));
}

static int64_t host_now(void)
{
    if (!s_have) {
        return 0;
    }
    return s_snap->now + (esp_timer_get_time() - s_rx_us) / 1000000;
}

static void fmt_clock(char *buf, size_t n, int64_t unix)
{
    time_t t = (time_t)(unix + s_snap->tz_offset);
    struct tm tm;
    gmtime_r(&t, &tm);
    snprintf(buf, n, "%02d:%02d", tm.tm_hour, tm.tm_min);
}

// Time left in a usage window, as claude-hud prints it: "33m", "4h 33m", "4d 14h".
static void fmt_reset(char *buf, size_t n, const dock_window_t *w, int64_t now)
{
    if (!w->present || w->resets_at == 0) {
        snprintf(buf, n, "—");
        return;
    }
    int64_t sec = w->resets_at - now;
    int mins = sec > 0 ? (int)((sec + 59) / 60) : 0;
    int h = mins / 60, m = mins % 60;
    if (mins < 60) {
        snprintf(buf, n, "%dm", mins);
    } else if (h >= 24) {
        if (h % 24) {
            snprintf(buf, n, "%dd %dh", h / 24, h % 24);
        } else {
            snprintf(buf, n, "%dd", h / 24);
        }
    } else if (m) {
        snprintf(buf, n, "%dh %dm", h, m);
    } else {
        snprintf(buf, n, "%dh", h);
    }
}

static bool is_active(dock_state_t st)
{
    return st == DOCK_APPROVAL || st == DOCK_INPUT || st == DOCK_ERROR || st == DOCK_RUNNING;
}

// "1:05", or "1h05m" from an hour on.
static void fmt_duration(char *buf, size_t n, int64_t sec)
{
    if (sec >= 3600) {
        snprintf(buf, n, "%dh%02dm", (int)(sec / 3600), (int)(sec % 3600 / 60));
    } else {
        snprintf(buf, n, "%d:%02d", (int)(sec / 60), (int)(sec % 60));
    }
}

// Running states count up; finished ones say how long ago.
static void fmt_elapsed(char *buf, size_t n, dock_state_t state, int64_t since, int64_t now)
{
    int64_t sec = now - since;
    if (sec < 0) {
        sec = 0;
    }
    if (is_active(state)) {
        fmt_duration(buf, n, sec);
    } else if (sec < 60) {
        snprintf(buf, n, "刚刚");
    } else if (sec < 3600) {
        snprintf(buf, n, "%d分钟前", (int)(sec / 60));
    } else if (sec < 86400) {
        snprintf(buf, n, "%d小时前", (int)(sec / 3600));
    } else {
        snprintf(buf, n, "%d天前", (int)(sec / 86400));
    }
}

static void fmt_tokens(char *buf, size_t n, int64_t v)
{
    double d = (double)v;
    if (v >= 1000000000) {
        snprintf(buf, n, "%.2fB", d / 1e9);
    } else if (v >= 10000000) {
        snprintf(buf, n, "%.1fM", d / 1e6);
    } else if (v >= 1000000) {
        snprintf(buf, n, "%.2fM", d / 1e6);
    } else if (v >= 10000) {
        snprintf(buf, n, "%.0fK", d / 1e3);
    } else if (v >= 1000) {
        snprintf(buf, n, "%.1fK", d / 1e3);
    } else {
        snprintf(buf, n, "%d", (int)v);
    }
}

// A session row's time: while the turn runs or waits for the user, its work
// time as Claude Code's spinner counts it (ticking on from turn_secs while
// running, held while waiting); otherwise fmt_elapsed.
static void fmt_row_time(char *buf, size_t n, const dock_session_t *s, int64_t now)
{
    bool turn = s->state == DOCK_RUNNING || s->state == DOCK_APPROVAL || s->state == DOCK_INPUT;
    if (!turn || s->turn_secs <= 0) {
        fmt_elapsed(buf, n, s->state, s->since, now);
        return;
    }
    int64_t sec = s->turn_secs;
    if (s->state == DOCK_RUNNING && now > s_snap->now) {
        sec += now - s_snap->now;
    }
    fmt_duration(buf, n, sec);
}

// The line under the project line: what the session is doing, else the
// assistant's latest text. "" leaves the project line alone.
static const char *row_text(const dock_session_t *s)
{
    return s->detail[0] ? s->detail : s->msg;
}

// Splits the sessions into pages by height, keeping each session together
// with its subagents.
static void layout_pages(void)
{
    int n = s_have ? s_snap->count : 0;
    int used = 0, mains = 0, subs = 0;
    s_page_count = 1;
    s_page_start[0] = 0;
    for (int i = 0; i < n; i++) {
        const dock_session_t *s = &s_snap->sessions[i];
        int h = MAIN_H + s->agent_count * (SUB_H + SUB_GAP);
        bool fits = mains == 0 || (used + GROUP_GAP + h <= TASKS_H && mains < MAX_MAIN && subs + s->agent_count <= MAX_SUB);
        if (!fits && s_page_count < MAX_PAGES) {
            s_page_start[s_page_count++] = i;
            used = mains = subs = 0;
        }
        used += (mains ? GROUP_GAP : 0) + h;
        mains++;
        subs += s->agent_count;
    }
    s_page_start[s_page_count] = n;
}

static int page_of(int index)
{
    for (int p = s_page_count - 1; p > 0; p--) {
        if (index >= s_page_start[p]) {
            return p;
        }
    }
    return 0;
}

// ---------- building ----------

// The strip on top: the computer's name and the summary on the left; the
// link dot and the clock on the right.
static void turn_host(int delta);

static void on_host_tap(lv_event_t *e)
{
    (void)e;
    turn_host(1);
}

static void build_strip(lv_obj_t *p)
{
    ui.title = line(p, font_get(FONT_BOLD, 22), C_TEXT, TITLE_MAX_W);
    lv_label_set_text(ui.title, "Agent");
    lv_obj_set_pos(ui.title, PAD_X, TOP);
    lv_obj_add_flag(ui.title, LV_OBJ_FLAG_CLICKABLE);
    lv_obj_set_ext_click_area(ui.title, 12);
    lv_obj_add_event_cb(ui.title, on_host_tap, LV_EVENT_CLICKED, NULL);

    // A dot per computer after the name, then a ring for the connection
    // page, the page shown drawn long; tapping them or the name shows the
    // next.
    ui.hosts = box(p, 0, 0, LV_SIZE_CONTENT, 6, 0, -1);
    lv_obj_set_flex_flow(ui.hosts, LV_FLEX_FLOW_ROW);
    lv_obj_set_flex_align(ui.hosts, LV_FLEX_ALIGN_START, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_column(ui.hosts, 5, 0);
    lv_obj_add_flag(ui.hosts, LV_OBJ_FLAG_CLICKABLE);
    lv_obj_set_ext_click_area(ui.hosts, 16);
    lv_obj_add_event_cb(ui.hosts, on_host_tap, LV_EVENT_CLICKED, NULL);
    for (int i = 0; i < LINK_HOSTS; i++) {
        ui.hdots[i] = box(ui.hosts, 0, 0, 6, 6, 0x3A3A3C, 3);
    }
    ui.hinfo = box(ui.hosts, 0, 0, 6, 6, 0xE9E9E5, 3);
    lv_obj_set_style_border_color(ui.hinfo, lv_color_hex(0x75746F), 0);
    lv_obj_add_flag(ui.hosts, LV_OBJ_FLAG_HIDDEN);

    ui.summary = line(p, font_get(FONT_REGULAR, 17), C_TEXT2, 360);
    lv_obj_align_to(ui.summary, ui.title, LV_ALIGN_OUT_RIGHT_BOTTOM, 12, -3);

    ui.clock = text(p, font_get(FONT_MONO, 22), C_TEXT);
    lv_label_set_text(ui.clock, "--:--");
    lv_obj_align(ui.clock, LV_ALIGN_TOP_RIGHT, -PAD_X, TOP + 2);

    ui.link_dot = box(p, 0, 0, 8, 8, C_GREEN, LV_RADIUS_CIRCLE);
    lv_obj_align_to(ui.link_dot, ui.clock, LV_ALIGN_OUT_LEFT_MID, -8, 0);
}

// "4h 33m / 5h" on the left, the percentage on the right, the bar below.
static void build_meter(lv_obj_t *row, meter_t *m, int x, const char *name)
{
    const int w = 174;
    m->reset = text(row, font_get(FONT_MONO, 19), 0xBDBDB8);
    lv_obj_set_pos(m->reset, x, 7);

    m->window = text(row, font_get(FONT_REGULAR, 17), C_TEXT3); // narrower than mono, leaves room for "100%"
    char buf[16];
    snprintf(buf, sizeof(buf), " / %s", name);
    lv_label_set_text(m->window, buf);

    m->pct = text(row, font_get(FONT_MONO_BOLD, 26), C_TEXT);
    lv_obj_set_width(m->pct, 80);
    lv_obj_set_style_text_align(m->pct, LV_TEXT_ALIGN_RIGHT, 0);
    lv_obj_set_pos(m->pct, x + w - 80, 0);

    m->bar = lv_bar_create(row);
    lv_obj_remove_style_all(m->bar);
    lv_obj_set_pos(m->bar, x, 34);
    lv_obj_set_size(m->bar, w, 7);
    lv_obj_set_style_bg_color(m->bar, lv_color_hex(C_TRACK), LV_PART_MAIN);
    lv_obj_set_style_bg_opa(m->bar, LV_OPA_COVER, LV_PART_MAIN);
    lv_obj_set_style_radius(m->bar, 4, LV_PART_MAIN);
    lv_obj_set_style_bg_opa(m->bar, LV_OPA_COVER, LV_PART_INDICATOR);
    lv_obj_set_style_radius(m->bar, 4, LV_PART_INDICATOR);
    lv_bar_set_range(m->bar, 0, 100);
}

static void build_usage(lv_obj_t *p)
{
    lv_obj_t *card = ui.usage_card = box(p, PAD_X, 54, SCR_W - 2 * PAD_X, 114, C_CARD, 18);
    for (int i = 0; i < 2; i++) {
        lv_obj_t *row = ui.usage[i].row = box(card, 16, 8 + i * 50, 532, 46, 0, -1);
        lv_obj_t *name = text(row, font_get(FONT_BOLD, 24), PROVIDER_COLOR[i]);
        lv_label_set_text(name, PROVIDER_NAME[i]);
        lv_obj_align(name, LV_ALIGN_LEFT_MID, 0, 0);
        build_meter(row, &ui.usage[i].m[0], 92, "5h");
        build_meter(row, &ui.usage[i].m[1], 280, "7d");

        ui.usage[i].today = text(row, font_get(FONT_MONO, 21), C_TEXT);
        lv_obj_set_width(ui.usage[i].today, 68);
        lv_obj_set_style_text_align(ui.usage[i].today, LV_TEXT_ALIGN_RIGHT, 0);
        lv_obj_set_pos(ui.usage[i].today, 464, 0);
        lv_obj_t *l = ui.usage[i].today_label = text(row, font_get(FONT_REGULAR, 16), C_TEXT4);
        lv_label_set_text(l, "今日");
        lv_obj_align(l, LV_ALIGN_BOTTOM_RIGHT, 0, 0);
    }
}

// The usage card's two layouts (§7.1): at 54, 114 high with two 46 px
// rows; with the line of readings at 78, 94 high with two 40 px rows and
// smaller numbers. Either way the tasks start at TASKS_Y.
static void layout_usage(bool lined)
{
    const lv_font_t *f26 = font_get(FONT_MONO_BOLD, 26), *pf = font_get(FONT_MONO_BOLD, lined ? 24 : 26);
    // What the smaller percentage moves up: its baseline, and its bottom.
    int up_base = (lv_font_get_line_height(f26) - f26->base_line) - (lv_font_get_line_height(pf) - pf->base_line);
    int up_line = lv_font_get_line_height(f26) - lv_font_get_line_height(pf);
    int row_h = lined ? 40 : 46;
    lv_obj_set_pos(ui.usage_card, PAD_X, lined ? 78 : 54);
    lv_obj_set_height(ui.usage_card, lined ? 94 : 114);
    for (int i = 0; i < 2; i++) {
        lv_obj_set_pos(ui.usage[i].row, 16, (lined ? 4 : 8) + i * (row_h + 4));
        lv_obj_set_height(ui.usage[i].row, row_h);
        for (int k = 0; k < 2; k++) {
            meter_t *m = &ui.usage[i].m[k];
            lv_obj_set_style_text_font(m->pct, pf, 0);
            lv_obj_set_y(m->reset, 7 - up_base);
            lv_obj_set_y(m->bar, 34 - up_line);
        }
        lv_obj_set_style_text_font(ui.usage[i].today, font_get(FONT_MONO, lined ? 19 : 21), 0);
        lv_obj_set_style_text_font(ui.usage[i].today_label, font_get(FONT_REGULAR, lined ? 14 : 16), 0);
    }
}

static lv_obj_t *spinner(lv_obj_t *parent, int size, int width)
{
    lv_obj_t *sp = lv_spinner_create(parent);
    lv_spinner_set_anim_params(sp, 1200, 270);
    lv_obj_set_size(sp, size, size);
    lv_obj_set_style_arc_width(sp, width, LV_PART_MAIN);
    lv_obj_set_style_arc_opa(sp, LV_OPA_TRANSP, LV_PART_MAIN);
    lv_obj_set_style_arc_width(sp, width, LV_PART_INDICATOR);
    lv_obj_set_style_arc_color(sp, lv_color_hex(C_GREEN), LV_PART_INDICATOR);
    lv_obj_set_style_arc_rounded(sp, true, LV_PART_INDICATOR);
    return sp;
}

// Session row; positions depend on its lines and are set when rendered.
static void build_row(lv_obj_t *p, row_t *r)
{
    r->obj = box(p, 0, 0, LIST_W, MAIN_H, C_CARD, 14);
    lv_obj_set_style_border_width(r->obj, 2, 0);
    lv_obj_set_style_border_opa(r->obj, LV_OPA_TRANSP, 0);

    r->chip = box(r->obj, 12, 0, CHIP, CHIP, 0, LV_RADIUS_CIRCLE);
    lv_obj_set_style_bg_opa(r->chip, 41, 0); // ~16% of the state color
    r->icon = text(r->chip, &lv_font_montserrat_20, C_TEXT);
    lv_obj_center(r->icon);
    r->spinner = spinner(r->chip, 24, 3);
    lv_obj_center(r->spinner);

    // Project name and provider tag, the state at the right end.
    r->project = line(r->obj, font_get(FONT_BOLD, 30), C_TEXT, LINE_W);
    r->prov = text(r->obj, font_get(FONT_BOLD, 16), C_TEXT);
    r->label = text(r->obj, font_get(FONT_BOLD, 22), C_TEXT);

    // Session name, the time and " ↓6.5K" at the right end.
    r->name = line(r->obj, font_get(FONT_REGULAR, 20), 0xC9C9C4, LINE_W);
    r->time = text(r->obj, font_get(FONT_MONO, 20), C_TEXT3);
    r->tok = text(r->obj, font_get(FONT_MONO, 20), C_TEXT5);

    r->text = line(r->obj, font_get(FONT_REGULAR, 24), C_TEXT2, LINE_W);
}

// Subagent row, indented under its session.
static void build_sub(lv_obj_t *p, sub_t *u)
{
    const int w = LIST_W - SUB_X;
    u->obj = box(p, SUB_X, 0, w, SUB_H, 0x0B0B0C, 10);
    u->icon = text(u->obj, &lv_font_montserrat_14, C_TEXT);
    lv_obj_align(u->icon, LV_ALIGN_LEFT_MID, 11, 0);
    u->spinner = spinner(u->obj, 16, 2);
    lv_obj_align(u->spinner, LV_ALIGN_LEFT_MID, 10, 0);
    u->type = text(u->obj, font_get(FONT_BOLD, 18), C_TEXT);
    lv_obj_align(u->type, LV_ALIGN_LEFT_MID, 36, 0);
    u->text = line(u->obj, font_get(FONT_REGULAR, 17), C_TEXT2, 200);
    u->time = text(u->obj, font_get(FONT_MONO, 16), C_TEXT3);
    lv_obj_set_width(u->time, SUB_TIME_W);
    lv_obj_set_style_text_align(u->time, LV_TEXT_ALIGN_RIGHT, 0);
    lv_obj_align(u->time, LV_ALIGN_RIGHT_MID, -12, 0);
    u->label = text(u->obj, font_get(FONT_BOLD, 17), C_TEXT);
    lv_obj_set_width(u->label, SUB_LABEL_W);
    lv_obj_set_style_text_align(u->label, LV_TEXT_ALIGN_RIGHT, 0);
    lv_obj_align(u->label, LV_ALIGN_RIGHT_MID, -12 - SUB_TIME_W - 6, 0);
}

static void go_page(int delta)
{
    int n = s_page_count;
    s_page = ((s_page + delta) % n + n) % n;
    s_first_id[0] = '\0'; // keep the page the user chose
}

static void render_rows(void);
static void render_strip(void);

static void on_pager(lv_event_t *e)
{
    (void)e;
    go_page(1);
    render_rows();
}

// Up and down page through the tasks, left and right through the
// computers and the connection page.
static void on_gesture(lv_event_t *e)
{
    (void)e;
    lv_indev_t *indev = lv_indev_active();
    lv_dir_t dir = lv_indev_get_gesture_dir(indev);
    lv_indev_wait_release(indev); // a swipe never also taps what it started on
    if ((dir == LV_DIR_TOP || dir == LV_DIR_BOTTOM) && !s_info) {
        go_page(dir == LV_DIR_TOP ? 1 : -1);
        render_rows();
    } else if (dir == LV_DIR_LEFT || dir == LV_DIR_RIGHT) {
        turn_host(dir == LV_DIR_LEFT ? 1 : -1);
    }
}

static void build_tasks(lv_obj_t *p)
{
    lv_obj_t *list = box(p, PAD_X, TASKS_Y, LIST_W, TASKS_H, 0, -1);
    for (int i = 0; i < MAX_MAIN; i++) {
        build_row(list, &ui.rows[i]);
    }
    for (int i = 0; i < MAX_SUB; i++) {
        build_sub(list, &ui.subs[i]);
    }

    ui.empty = box(p, PAD_X, TASKS_Y, SCR_W - 2 * PAD_X, TASKS_H, 0x0B0B0C, 18);
    lv_obj_t *t = text(ui.empty, font_get(FONT_BOLD, 26), C_TEXT);
    lv_label_set_text(t, "没有进行中的任务");
    lv_obj_align(t, LV_ALIGN_CENTER, 0, -16);
    lv_obj_t *s = text(ui.empty, font_get(FONT_REGULAR, 18), C_TEXT3);
    lv_label_set_text(s, "Claude 和 Codex 都在待命");
    lv_obj_align(s, LV_ALIGN_CENTER, 0, 24);

    ui.pager = box(p, SCR_W - PAD_X - 16, TASKS_Y, 16, TASKS_H, 0, -1);
    lv_obj_add_flag(ui.pager, LV_OBJ_FLAG_CLICKABLE);
    lv_obj_set_ext_click_area(ui.pager, 16);
    lv_obj_add_event_cb(ui.pager, on_pager, LV_EVENT_CLICKED, NULL);
    lv_obj_set_flex_flow(ui.pager, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_flex_align(ui.pager, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_row(ui.pager, 6, 0);
    for (int i = 0; i < MAX_DOTS; i++) {
        ui.dots[i] = box(ui.pager, 0, 0, 6, 6, 0x3A3A3C, 3);
        lv_obj_remove_flag(ui.dots[i], LV_OBJ_FLAG_CLICKABLE);
    }
}

static void on_ota_answer(lv_event_t *e)
{
    ota_answer(lv_event_get_user_data(e) != NULL);
}

// One of the two answers to an update over WiFi, at the bottom of the card.
static lv_obj_t *ov_button(lv_obj_t *p, const char *label, uint32_t bg, uint32_t pressed, uint32_t fg, bool yes)
{
    lv_obj_t *b = box(p, 0, 0, 150, 48, bg, 14);
    lv_obj_add_flag(b, LV_OBJ_FLAG_CLICKABLE);
    lv_obj_set_ext_click_area(b, 8);
    lv_obj_set_style_bg_color(b, lv_color_hex(pressed), LV_STATE_PRESSED);
    lv_obj_add_event_cb(b, on_ota_answer, LV_EVENT_CLICKED, yes ? (void *)1 : NULL);
    lv_obj_t *l = text(b, font_get(FONT_BOLD, 20), fg);
    lv_label_set_text(l, label);
    lv_obj_center(l);
    lv_obj_align(b, yes ? LV_ALIGN_BOTTOM_RIGHT : LV_ALIGN_BOTTOM_LEFT, yes ? -32 : 32, -16);
    lv_obj_add_flag(b, LV_OBJ_FLAG_HIDDEN);
    return b;
}

// The card over the dimmed screen: why there is nothing current to show,
// a firmware update's progress, or the question before one over WiFi; the
// firmware version at the bottom.
static void build_overlay(lv_obj_t *p)
{
    ui.overlay = box(p, 0, 0, 380, 204, 0x161617, 22);
    lv_obj_center(ui.overlay);
    lv_obj_set_style_border_width(ui.overlay, 1, 0);
    lv_obj_set_style_border_color(ui.overlay, lv_color_hex(0x2C2C2E), 0);
    ui.ov_icon = text(ui.overlay, &lv_font_montserrat_28, C_RED);
    lv_obj_align(ui.ov_icon, LV_ALIGN_TOP_MID, 0, 18);
    ui.ov_title = text(ui.overlay, font_get(FONT_BOLD, 28), C_TEXT);
    lv_obj_align(ui.ov_title, LV_ALIGN_TOP_MID, 0, 54);
    ui.ov_sub = line(ui.overlay, font_get(FONT_REGULAR, 17), C_TEXT2, 340);
    lv_obj_set_style_text_align(ui.ov_sub, LV_TEXT_ALIGN_CENTER, 0);
    lv_obj_align(ui.ov_sub, LV_ALIGN_TOP_MID, 0, 100);
    ui.ov_note = text(ui.overlay, font_get(FONT_REGULAR, 15), C_TEXT4);
    lv_obj_align(ui.ov_note, LV_ALIGN_TOP_MID, 0, 130);

    ui.ov_bar = lv_bar_create(ui.overlay);
    lv_obj_remove_style_all(ui.ov_bar);
    lv_obj_set_size(ui.ov_bar, 300, 8);
    lv_obj_align(ui.ov_bar, LV_ALIGN_TOP_MID, 0, 138);
    lv_obj_set_style_bg_color(ui.ov_bar, lv_color_hex(C_TRACK), LV_PART_MAIN);
    lv_obj_set_style_bg_opa(ui.ov_bar, LV_OPA_COVER, LV_PART_MAIN);
    lv_obj_set_style_radius(ui.ov_bar, 4, LV_PART_MAIN);
    lv_obj_set_style_bg_color(ui.ov_bar, lv_color_hex(C_GREEN), LV_PART_INDICATOR);
    lv_obj_set_style_bg_opa(ui.ov_bar, LV_OPA_COVER, LV_PART_INDICATOR);
    lv_obj_set_style_radius(ui.ov_bar, 4, LV_PART_INDICATOR);
    lv_bar_set_range(ui.ov_bar, 0, 100);

    ui.ov_ver = text(ui.overlay, font_get(FONT_REGULAR, 13), C_TEXT5);
    lv_obj_align(ui.ov_ver, LV_ALIGN_BOTTOM_MID, 0, -14);

    ui.ov_no = ov_button(ui.overlay, "取消", 0x2C2C2E, 0x3A3A3C, C_TEXT, false);
    ui.ov_yes = ov_button(ui.overlay, "升级", C_GREEN, 0x2FB574, 0x0B0B0C, true);
}

// Reading icons, as the web preview's SCR_ICONS draws them on a 24-unit
// grid (coordinates here in tenths), with straight strokes and arcs only.
typedef struct {
    bool arc;
    int16_t v[5]; // line: x1 y1 x2 y2; arc: centre x y, radius, start and end angle (clockwise from 3 o'clock)
} stroke_t;

#define LINE(x1, y1, x2, y2) {false, {x1, y1, x2, y2, 0}}
#define ARC(cx, cy, r, a0, a1) {true, {cx, cy, r, a0, a1}}

static const stroke_t ICON_CPU[] = {
    LINE(70, 70, 170, 70), LINE(170, 70, 170, 170), LINE(170, 170, 70, 170), LINE(70, 170, 70, 70),
    LINE(100, 100, 140, 100), LINE(140, 100, 140, 140), LINE(140, 140, 100, 140), LINE(100, 140, 100, 100),
    LINE(100, 30, 100, 70), LINE(140, 30, 140, 70), LINE(100, 170, 100, 210), LINE(140, 170, 140, 210),
    LINE(30, 100, 70, 100), LINE(30, 140, 70, 140), LINE(170, 100, 210, 100), LINE(170, 140, 210, 140),
};
static const stroke_t ICON_GPU[] = {
    LINE(25, 60, 215, 60), LINE(215, 60, 215, 170), LINE(215, 170, 25, 170), LINE(25, 170, 25, 60),
    ARC(90, 115, 30, 0, 360), LINE(150, 95, 185, 95), LINE(150, 135, 185, 135), LINE(55, 170, 55, 200), LINE(95, 170, 95, 200),
};
static const stroke_t ICON_MEMORY[] = {
    LINE(30, 70, 210, 70), LINE(210, 70, 210, 160), LINE(210, 160, 30, 160), LINE(30, 160, 30, 70),
    LINE(65, 100, 95, 100), LINE(95, 100, 95, 130), LINE(95, 130, 65, 130), LINE(65, 130, 65, 100),
    LINE(145, 100, 175, 100), LINE(175, 100, 175, 130), LINE(175, 130, 145, 130), LINE(145, 130, 145, 100),
    LINE(60, 160, 60, 190), LINE(100, 160, 100, 190), LINE(140, 160, 140, 190), LINE(180, 160, 180, 190),
};
// A ring, the hub and three blades, each two arcs meeting at its base and tip.
static const stroke_t ICON_ROTOR[] = {
    ARC(120, 120, 100, 0, 360), ARC(120, 120, 16, 0, 360),
    ARC(144, 72, 40, 127, 233), ARC(96, 72, 40, 307, 53),
    ARC(150, 165, 40, 247, 353), ARC(174, 123, 40, 67, 173),
    ARC(66, 123, 40, 7, 113), ARC(90, 165, 40, 187, 293),
};
// A thermometer: the tube with its round top, the bulb open where they
// meet, and two marks beside it.
static const stroke_t ICON_TEMP[] = {
    LINE(125, 138, 125, 50), ARC(100, 50, 25, 180, 360), LINE(75, 50, 75, 138), ARC(100, 175, 45, 304, 236),
    LINE(155, 60, 195, 60),  LINE(155, 105, 195, 105),
};
static const stroke_t ICON_DISK[] = {
    LINE(40, 45, 200, 45), LINE(200, 45, 200, 195), LINE(200, 195, 40, 195), LINE(40, 195, 40, 45),
    LINE(40, 140, 200, 140), ARC(165, 168, 10, 0, 360),
};

#define STROKES(a) {a, sizeof(a) / sizeof(a[0])}
static const struct {
    const stroke_t *s;
    int n;
} ICONS[DOCK_ICON_COUNT] = {
    [DOCK_ICON_NONE] = {NULL, 0},          [DOCK_ICON_CPU] = STROKES(ICON_CPU),
    [DOCK_ICON_GPU] = STROKES(ICON_GPU),   [DOCK_ICON_MEMORY] = STROKES(ICON_MEMORY),
    [DOCK_ICON_ROTOR] = STROKES(ICON_ROTOR), [DOCK_ICON_TEMP] = STROKES(ICON_TEMP),
    [DOCK_ICON_DISK] = STROKES(ICON_DISK),
};

// A reading is coloured by its icon.
static const uint32_t ICON_COLOR[DOCK_ICON_COUNT] = {
    [DOCK_ICON_NONE] = 0xBDBDB8,   [DOCK_ICON_CPU] = 0x5AA9FF,   [DOCK_ICON_GPU] = 0xC084FC,
    [DOCK_ICON_MEMORY] = 0x3DD68C, [DOCK_ICON_ROTOR] = 0x4FD1C5, [DOCK_ICON_TEMP] = 0xF472B6, [DOCK_ICON_DISK] = 0xBDBDB8,
};

// Draws icon in a size px square at ox, oy, in obj's arc and line colours.
static void draw_strokes(lv_layer_t *layer, lv_obj_t *obj, dock_icon_t icon, int32_t ox, int32_t oy, int size)
{
    int w = (size * 2 + 12) / 24; // stroke 2 of 24
    lv_draw_arc_dsc_t arc;
    lv_draw_arc_dsc_init(&arc);
    arc.base.layer = layer;
    lv_obj_init_draw_arc_dsc(obj, LV_PART_MAIN, &arc);
    arc.width = w;
    arc.rounded = 1;
    lv_draw_line_dsc_t ln;
    lv_draw_line_dsc_init(&ln);
    ln.base.layer = layer;
    lv_obj_init_draw_line_dsc(obj, LV_PART_MAIN, &ln);
    ln.width = w;
    ln.round_start = ln.round_end = 1;

    for (int i = 0; i < ICONS[icon].n; i++) {
        const int16_t *v = ICONS[icon].s[i].v;
        if (ICONS[icon].s[i].arc) {
            arc.center.x = ox + v[0] * size / 240;
            arc.center.y = oy + v[1] * size / 240;
            arc.radius = (uint16_t)(v[2] * size / 240 + w / 2); // the stroke centred on the radius
            arc.start_angle = v[3];
            arc.end_angle = v[4];
            lv_draw_arc(layer, &arc);
        } else {
            ln.p1.x = ox + v[0] * size / 240;
            ln.p1.y = oy + v[1] * size / 240;
            ln.p2.x = ox + v[2] * size / 240;
            ln.p2.y = oy + v[3] * size / 240;
            lv_draw_line(layer, &ln);
        }
    }
}

// A reading's icon (its user data) fills its object.
static void draw_icon(lv_event_t *e)
{
    lv_obj_t *o = lv_event_get_current_target_obj(e);
    lv_area_t a;
    lv_obj_get_coords(o, &a);
    draw_strokes(lv_event_get_layer(e), o, (dock_icon_t)(intptr_t)lv_obj_get_user_data(o), a.x1, a.y1, lv_area_get_width(&a));
}

// A reading's colour: its icon's, amber or red when its level says so.
static uint32_t reading_color(const dock_reading_t *r)
{
    if (r->level == DOCK_LEVEL_CRIT) {
        return C_RED;
    }
    if (r->level == DOCK_LEVEL_WARN) {
        return C_AMBER;
    }
    return ICON_COLOR[r->icon];
}

// The panel level for the brightness set.
static uint8_t full_level(void)
{
    return (uint8_t)(settings_get().brightness * 255 / 100);
}

static void build_line(lv_obj_t *p)
{
    ui.line = box(p, READ_X, READ_Y, READ_W, READ_H, 0, -1); // clips the groups sliding past its ends
    for (int g = 0; g < 2; g++) {
        ui.lgroup[g] = box(ui.line, 0, 0, READ_W, READ_H, 0, -1);
        for (int i = 0; i < READ_N; i++) {
            seg_t *s = &ui.seg[g][i];
            s->obj = box(ui.lgroup[g], 0, 0, 10, READ_H, 0, -1);
            s->icon = box(s->obj, 0, (READ_H - READ_ICON) / 2, READ_ICON, READ_ICON, 0, -1);
            lv_obj_set_style_arc_width(s->icon, 1, 0); // icons are drawn in these styles
            lv_obj_set_style_line_width(s->icon, 1, 0);
            lv_obj_add_event_cb(s->icon, draw_icon, LV_EVENT_DRAW_MAIN_END, NULL);
            s->label = text(s->obj, font_get(FONT_REGULAR, 18), C_TEXT2);
            s->value = text(s->obj, font_get(FONT_MONO_BOLD, 24), C_TEXT);
            s->unit = text(s->obj, font_get(FONT_MONO, 15), C_TEXT3);
            s->bar = lv_bar_create(s->obj);
            lv_obj_remove_style_all(s->bar);
            lv_obj_set_style_bg_color(s->bar, lv_color_hex(C_TRACK), LV_PART_MAIN);
            lv_obj_set_style_bg_opa(s->bar, LV_OPA_COVER, LV_PART_MAIN);
            lv_obj_set_style_radius(s->bar, 3, LV_PART_MAIN);
            lv_obj_set_style_bg_opa(s->bar, LV_OPA_COVER, LV_PART_INDICATOR);
            lv_obj_set_style_radius(s->bar, 3, LV_PART_INDICATOR);
            lv_bar_set_range(s->bar, 0, 100);
            s->sub = line(s->obj, font_get(FONT_REGULAR, 16), C_TEXT2, 10);
            lv_obj_set_style_text_align(s->sub, LV_TEXT_ALIGN_RIGHT, 0);
        }
    }
    lv_obj_add_flag(ui.line, LV_OBJ_FLAG_HIDDEN);
}

// The connection page, in place of the Agent page under the strip: the
// screen's address and pairing code large, what to do with them, then the
// computers on the screen and its WiFi.
static void build_info(lv_obj_t *p)
{
    const int w = SCR_W - 2 * PAD_X;
    ui.info = box(p, 0, 0, SCR_W, SCR_H, 0, -1);
    lv_obj_remove_flag(ui.info, LV_OBJ_FLAG_CLICKABLE); // over the strip: taps go through to the name
    lv_obj_t *card = box(ui.info, PAD_X, 54, w, 238, C_CARD, 18);
    lv_obj_t *l = text(card, font_get(FONT_REGULAR, 17), C_TEXT3);
    lv_label_set_text(l, "屏幕地址");
    lv_obj_set_pos(l, 24, 18);
    ui.info_addr = text(card, font_get(FONT_MONO_BOLD, 52), C_TEXT);
    lv_obj_set_pos(ui.info_addr, 24, 40);
    l = text(card, font_get(FONT_REGULAR, 17), C_TEXT3);
    lv_label_set_text(l, "配对码");
    lv_obj_set_pos(l, 24, 124);
    ui.info_code = text(card, font_get(FONT_MONO_BOLD, 52), C_GREEN);
    lv_obj_set_style_text_letter_space(ui.info_code, 8, 0);
    lv_obj_set_pos(ui.info_code, 24, 146);

    // What to do with them, under the card; who is on the screen and its
    // WiFi at the bottom.
    ui.info_hint = line(ui.info, font_get(FONT_REGULAR, 18), C_TEXT2, w - 8);
    lv_obj_set_pos(ui.info_hint, PAD_X + 4, 306);
    ui.info_hosts = line(ui.info, font_get(FONT_REGULAR, 18), C_TEXT, w - 8);
    lv_obj_set_pos(ui.info_hosts, PAD_X + 4, 372);
    ui.info_wifi = line(ui.info, font_get(FONT_REGULAR, 16), C_TEXT4, w - 8);
    lv_obj_set_pos(ui.info_wifi, PAD_X + 4, 404);
    lv_obj_add_flag(ui.info, LV_OBJ_FLAG_HIDDEN);
}

void ui_create(void)
{
    s_snap = heap_caps_calloc(1, sizeof(dock_snapshot_t), MALLOC_CAP_SPIRAM);

    lv_obj_t *scr = lv_screen_active();
    lv_obj_remove_style_all(scr);
    lv_obj_set_style_bg_color(scr, lv_color_black(), 0);
    lv_obj_set_style_bg_opa(scr, LV_OPA_COVER, 0);
    lv_obj_remove_flag(scr, LV_OBJ_FLAG_SCROLLABLE);
    lv_obj_add_event_cb(scr, on_gesture, LV_EVENT_GESTURE, NULL);

    ui.root = box(scr, 0, 0, SCR_W, SCR_H, 0, -1);
    ui.content = box(ui.root, 0, 0, SCR_W, SCR_H, 0, -1);
    build_strip(ui.content);
    ui.agent = box(ui.content, 0, 0, SCR_W, SCR_H, 0, -1);
    lv_obj_remove_flag(ui.agent, LV_OBJ_FLAG_CLICKABLE); // over the strip: taps go through to the name
    build_line(ui.agent);
    build_usage(ui.agent);
    build_tasks(ui.agent);
    build_info(ui.content);
    build_overlay(ui.root);

    int64_t now = esp_timer_get_time();
    s_touch_us = s_active_us = s_shift_us = now;
    render_rows();
    render_strip();
}

// ---------- rendering ----------

// The Agent page's summary: the active tasks by state.
static void agent_summary(char *buf, size_t size)
{
    buf[0] = '\0';
    if (s_have) {
        static const dock_state_t order[] = {DOCK_APPROVAL, DOCK_INPUT, DOCK_ERROR, DOCK_RUNNING};
        size_t used = 0;
        for (size_t i = 0; i < sizeof(order) / sizeof(order[0]); i++) {
            int n = s_snap->counts[order[i]];
            if (n > 0 && used < size) {
                used += snprintf(buf + used, size - used, "%s%s %d", used ? " · " : "", STATE[order[i]].label, n);
            }
        }
        if (!used && s_snap->count > 0) {
            snprintf(buf, size, "全部完成");
        }
    }
}

static void show(lv_obj_t *o, bool on)
{
    if (on) {
        lv_obj_remove_flag(o, LV_OBJ_FLAG_HIDDEN);
    } else {
        lv_obj_add_flag(o, LV_OBJ_FLAG_HIDDEN);
    }
}

// The strip: the computer's name (from its go-dock's hello), the dots of
// the pages, and the active tasks, the summary taking the room up to the
// link dot. On the connection page its title, no summary.
static void render_strip(void)
{
    char buf[100] = "";
    const char *name = s_info ? "连接信息" : s_shown_name[0] ? s_shown_name : "Agent";
    if (!s_info) {
        agent_summary(buf, sizeof(buf));
    }
    lv_point_t size;
    lv_text_get_size(&size, name, lv_obj_get_style_text_font(ui.title, 0), 0, 0, LV_COORD_MAX, LV_TEXT_FLAG_NONE);
    lv_obj_set_width(ui.title, size.x < TITLE_MAX_W ? size.x : TITLE_MAX_W);
    set_text(ui.title, name);
    set_text(ui.summary, buf);

    // The dots show once there is another page: a computer's has the
    // connection page beside it.
    bool many = s_order_n > 0 || s_info;
    show(ui.hosts, many);
    lv_obj_set_width(ui.hinfo, s_info ? 18 : 6);
    lv_obj_set_style_bg_opa(ui.hinfo, s_info ? LV_OPA_COVER : LV_OPA_TRANSP, 0);
    lv_obj_set_style_border_width(ui.hinfo, s_info ? 0 : 1, 0);
    for (int i = 0; many && i < LINK_HOSTS; i++) {
        show(ui.hdots[i], i < s_order_n);
        if (i >= s_order_n) {
            continue;
        }
        // Another computer's dot takes the colour of what needs the user there.
        const link_host_t *h = &s_hosts[s_order[i]];
        bool on = !s_info && s_order[i] == s_shown;
        uint32_t c = on ? 0xE9E9E5 : h->attention ? STATE[h->top].color : 0x3A3A3C;
        lv_obj_set_width(ui.hdots[i], on ? 18 : 6);
        lv_obj_set_style_bg_color(ui.hdots[i], lv_color_hex(c), 0);
    }
    lv_obj_update_layout(ui.content);
    lv_obj_t *after = ui.title;
    if (many) {
        lv_obj_align_to(ui.hosts, ui.title, LV_ALIGN_OUT_RIGHT_MID, 12, 1);
        after = ui.hosts;
    }
    lv_obj_align_to(ui.summary, after, many ? LV_ALIGN_OUT_RIGHT_MID : LV_ALIGN_OUT_RIGHT_BOTTOM, 12, many ? -1 : -3);
    if (many) {
        // On the title's baseline, like without the dots.
        lv_obj_set_y(ui.summary, lv_obj_get_y(ui.title) + lv_obj_get_height(ui.title) - lv_obj_get_height(ui.summary) - 3);
    }
    int w = lv_obj_get_x(ui.link_dot) - 12 - lv_obj_get_x(ui.summary);
    lv_obj_set_width(ui.summary, w > 1 ? w : 1);
}

// Lays out reading r in seg s at x, w wide: icon, label, the reading and
// its unit on one baseline, then the bar taking the rest (at least
// READ_BAR), or without a percent the note right-aligned there.
static void fill_seg(seg_t *s, const dock_reading_t *r, int x, int w)
{
    show(s->obj, r != NULL);
    if (!r) {
        return;
    }
    uint32_t c = reading_color(r);
    lv_obj_set_pos(s->obj, x, 0);
    lv_obj_set_width(s->obj, w);
    lv_obj_set_user_data(s->icon, (void *)(intptr_t)r->icon);
    lv_obj_set_style_arc_color(s->icon, lv_color_hex(c), 0);
    lv_obj_set_style_line_color(s->icon, lv_color_hex(c), 0);
    lv_obj_invalidate(s->icon);
    show(s->icon, r->icon != DOCK_ICON_NONE);
    set_text(s->label, r->label);
    set_text(s->value, r->text);
    set_color(s->value, r->level ? c : C_TEXT);
    set_text(s->unit, r->unit);
    show(s->unit, r->unit[0]);
    lv_obj_update_layout(s->obj);

    int px = r->icon != DOCK_ICON_NONE ? READ_ICON + 6 : 0;
    on_baseline(s->label, px, READ_BASE);
    px += lv_obj_get_width(s->label) + 6;
    on_baseline(s->value, px, READ_BASE);
    px += lv_obj_get_width(s->value);
    if (r->unit[0]) {
        on_baseline(s->unit, px + 1, READ_BASE);
        px += 1 + lv_obj_get_width(s->unit);
    }
    px += 6;
    int rest = w - px;
    show(s->bar, r->pct >= 0);
    show(s->sub, r->pct < 0 && r->sub[0]);
    if (r->pct >= 0) {
        lv_obj_set_pos(s->bar, px, (READ_H - 6) / 2);
        lv_obj_set_size(s->bar, rest > READ_BAR ? rest : READ_BAR, 6);
        lv_obj_set_style_bg_color(s->bar, lv_color_hex(c), LV_PART_INDICATOR);
        lv_bar_set_value(s->bar, r->pct < 2 ? 2 : r->pct, LV_ANIM_OFF);
    } else if (r->sub[0]) {
        lv_obj_set_width(s->sub, rest > 1 ? rest : 1);
        set_text(s->sub, r->sub);
        on_baseline(s->sub, px, READ_BASE);
    }
}

// Fills group object g with group k of the readings, on cols columns.
static void fill_group(int g, int k, int cols)
{
    int w = (READ_W - (cols - 1) * READ_GAP) / cols;
    for (int i = 0; i < READ_N; i++) {
        int n = k * READ_N + i;
        fill_seg(&ui.seg[g][i], i < cols && n < s_line.count ? &s_line.items[n] : NULL, i * (w + READ_GAP), w);
    }
}

static void set_x(lv_obj_t *o, int32_t x)
{
    if (lv_obj_get_x(o) != x) {
        lv_obj_set_x(o, x);
    }
}

// Moves the line on, from the clock: group k stays, then k + 1 slides in
// from the right, eased in and out as in the web preview.
static void tick_line(int64_t now_us)
{
    int n = s_line.count;
    if (n == 0) {
        return;
    }
    int groups = (n + READ_N - 1) / READ_N, cols = groups > 1 ? READ_N : n;
    int k = 0;
    int32_t off = 0;
    if (groups > 1) {
        int64_t period = READ_DWELL_US + READ_SLIDE_US, e = now_us - s_line_start_us;
        if (e < 0) {
            e = 0;
        }
        k = (int)(e / period % groups);
        int64_t r = e % period;
        if (r >= READ_DWELL_US) {
            float x = (float)(r - READ_DWELL_US) / READ_SLIDE_US, y = 2 - 2 * x;
            float eased = x < 0.5f ? 4 * x * x * x : 1 - y * y * y / 2;
            off = (int32_t)(eased * (READ_W + READ_GAP) + 0.5f);
        }
    }
    int want[2] = {k, off ? (k + 1) % groups : -1};
    for (int g = 0; g < 2; g++) {
        if (want[g] < 0) {
            show(ui.lgroup[g], false);
            continue;
        }
        if (want[g] != s_lshown[g]) {
            fill_group(g, want[g], cols);
            s_lshown[g] = want[g];
        }
        set_x(ui.lgroup[g], g == 0 ? -off : READ_W + READ_GAP - off);
        show(ui.lgroup[g], true);
    }
}

// New readings: texts and bars change in place; other readings (their ids)
// start the turning over; the line coming or going relays the Agent page.
static void take_line(int64_t now_us)
{
    if (s_shown < 0 || !link_take_line(s_shown, s_shown_gen, &s_line, &s_line_version)) {
        return;
    }
    char ids[sizeof(s_line_ids)] = "";
    for (int i = 0; i < s_line.count; i++) {
        size_t used = strlen(ids);
        snprintf(ids + used, sizeof(ids) - used, "%s,", s_line.items[i].id);
    }
    if (strcmp(ids, s_line_ids) != 0) {
        snprintf(s_line_ids, sizeof(s_line_ids), "%s", ids);
        s_line_start_us = now_us;
    }
    s_lshown[0] = s_lshown[1] = -1;
    bool lined = s_line.count > 0;
    if (lined != s_lined) {
        s_lined = lined;
        layout_usage(lined);
        show(ui.line, lined);
    }
}

static void render_usage(int64_t now)
{
    char buf[24];
    for (int i = 0; i < 2; i++) {
        const dock_usage_t *u = &s_snap->usage[i];
        const dock_window_t *w[2] = {&u->five_hour, &u->seven_day};
        for (int k = 0; k < 2; k++) {
            meter_t *m = &ui.usage[i].m[k];
            if (s_have && w[k]->present) {
                int pct = (int)(w[k]->pct + 0.5f);
                snprintf(buf, sizeof(buf), "%d%%", pct);
                set_text(m->pct, buf);
                uint32_t c = pct >= 90 ? C_RED : pct >= 70 ? C_AMBER : PROVIDER_COLOR[i];
                lv_obj_set_style_bg_color(m->bar, lv_color_hex(c), LV_PART_INDICATOR);
                lv_bar_set_value(m->bar, pct < 2 ? 2 : pct, LV_ANIM_OFF);
            } else {
                set_text(m->pct, "—");
                lv_bar_set_value(m->bar, 0, LV_ANIM_OFF);
            }
            fmt_reset(buf, sizeof(buf), w[k], now);
            set_text(m->reset, buf);
            lv_obj_align_to(m->window, m->reset, LV_ALIGN_OUT_RIGHT_BOTTOM, 0, 0);
        }
        fmt_tokens(buf, sizeof(buf), s_have ? u->today_tokens : 0);
        set_text(ui.usage[i].today, buf);
    }
}

static void show_state_icon(lv_obj_t *icon, lv_obj_t *spin, dock_state_t st, uint32_t color)
{
    if (STATE[st].icon) {
        lv_obj_add_flag(spin, LV_OBJ_FLAG_HIDDEN);
        lv_obj_remove_flag(icon, LV_OBJ_FLAG_HIDDEN);
        set_text(icon, STATE[st].icon);
        set_color(icon, color);
    } else {
        lv_obj_add_flag(icon, LV_OBJ_FLAG_HIDDEN);
        lv_obj_remove_flag(spin, LV_OBJ_FLAG_HIDDEN);
    }
}

// Lays out one session row at y and returns its height.
static int render_main(row_t *r, const dock_session_t *s, int y, int64_t now)
{
    const char *text = row_text(s);
    int h = MAIN_H;
    uint32_t color = STATE[s->state].color;
    char buf[32];

    lv_obj_remove_flag(r->obj, LV_OBJ_FLAG_HIDDEN);
    lv_obj_set_pos(r->obj, 0, y);
    lv_obj_set_height(r->obj, h);
    lv_obj_set_style_bg_color(r->obj, lv_color_hex(STATE[s->state].row_bg), 0);
    lv_obj_set_style_border_color(r->obj, lv_color_hex(color), 0);
    lv_obj_set_style_border_opa(r->obj, STATE[s->state].ring ? 115 : LV_OPA_TRANSP, 0);

    lv_obj_set_y(r->chip, (h - CHIP) / 2);
    lv_obj_set_style_bg_color(r->chip, lv_color_hex(color), 0);
    show_state_icon(r->icon, r->spinner, s->state, color);
    lv_obj_center(r->icon);

    // First line: project name and provider tag, the state at the right
    // end. Second: the session name, then this turn's time and output tokens
    // ("1:55 ↓6.5K", like Claude Code's spinner) at the right end. Only the
    // names give way.
    set_text(r->prov, PROVIDER_NAME[s->provider]);
    set_color(r->prov, PROVIDER_COLOR[s->provider]);
    set_text(r->label, STATE[s->state].label);
    set_color(r->label, color);
    fmt_row_time(buf, sizeof(buf), s, now);
    set_text(r->time, buf);
    if (s->turn_output > 0) {
        char n[16];
        fmt_tokens(n, sizeof(n), s->turn_output);
        snprintf(buf, sizeof(buf), " ↓%s", n);
        set_text(r->tok, buf);
        lv_obj_remove_flag(r->tok, LV_OBJ_FLAG_HIDDEN);
    } else {
        lv_obj_add_flag(r->tok, LV_OBJ_FLAG_HIDDEN);
    }
    lv_obj_set_width(r->project, LV_SIZE_CONTENT);
    set_text(r->project, s->project[0] ? s->project : "-");
    lv_obj_set_width(r->name, LV_SIZE_CONTENT);
    set_text(r->name, s->title);
    lv_obj_update_layout(r->obj);

    // Baselines of the three lines; without text the first two are centred.
    int b1 = text[0] ? 41 : 59, b2 = b1 + 32, b3 = b2 + 35;

    int label_x = END_X - lv_obj_get_width(r->label);
    int room = label_x - 12 - MID_X - 8 - lv_obj_get_width(r->prov);
    int pw = lv_obj_get_width(r->project);
    if (pw > room) {
        pw = room > 1 ? room : 1;
        lv_obj_set_width(r->project, pw);
    }
    on_baseline(r->project, MID_X, b1);
    on_baseline(r->prov, MID_X + pw + 8, b1);
    on_baseline(r->label, label_x, b1);

    int time_x = END_X - lv_obj_get_width(r->time) - (s->turn_output > 0 ? lv_obj_get_width(r->tok) : 0);
    room = time_x - 12 - MID_X;
    if (lv_obj_get_width(r->name) > room) {
        lv_obj_set_width(r->name, room > 1 ? room : 1);
    }
    on_baseline(r->name, MID_X, b2);
    show(r->name, s->title[0]);
    on_baseline(r->time, time_x, b2);
    on_baseline(r->tok, time_x + lv_obj_get_width(r->time), b2);

    set_text(r->text, text);
    on_baseline(r->text, MID_X, b3);
    show(r->text, text[0]);
    return h;
}

static void render_sub(sub_t *u, const dock_agent_t *a, int y, int64_t now)
{
    static const uint32_t bg[DOCK_STATE_COUNT] = {
        [DOCK_APPROVAL] = 0x1C1305, [DOCK_INPUT] = 0x0B0B0C, [DOCK_ERROR] = 0x1A0A08,
        [DOCK_RUNNING] = 0x0B0B0C, [DOCK_DONE] = 0x0B0B0C, [DOCK_IDLE] = 0x0B0B0C,
    };
    uint32_t color = STATE[a->state].color;
    char buf[32];

    lv_obj_remove_flag(u->obj, LV_OBJ_FLAG_HIDDEN);
    lv_obj_set_y(u->obj, y);
    lv_obj_set_style_bg_color(u->obj, lv_color_hex(bg[a->state]), 0);
    show_state_icon(u->icon, u->spinner, a->state, color);

    set_text(u->type, a->type[0] ? a->type : "agent");
    fmt_elapsed(buf, sizeof(buf), a->state, a->since, now);
    set_text(u->time, buf);
    // Running subagents show only the spinner; others name their state.
    bool labelled = a->state != DOCK_RUNNING;
    if (labelled) {
        set_text(u->label, STATE[a->state].label);
        set_color(u->label, color);
        lv_obj_remove_flag(u->label, LV_OBJ_FLAG_HIDDEN);
    } else {
        lv_obj_add_flag(u->label, LV_OBJ_FLAG_HIDDEN);
    }
    lv_obj_update_layout(u->obj);
    int x = lv_obj_get_x(u->type) + lv_obj_get_width(u->type) + 8;
    int end = LIST_W - SUB_X - 12 - SUB_TIME_W - 6 - (labelled ? SUB_LABEL_W + 6 : 0);
    lv_obj_set_width(u->text, end > x ? end - x : 1);
    set_text(u->text, a->text);
    lv_obj_align(u->text, LV_ALIGN_LEFT_MID, x, 0);
}

static void render_rows(void)
{
    int n = s_have ? s_snap->count : 0;
    layout_pages();
    if (s_page >= s_page_count) {
        s_page = s_page_count - 1;
    }
    int64_t now = host_now();
    int from = s_page_start[s_page], to = s_page_start[s_page + 1];
    int y = 0, mains = 0, subs = 0;

    for (int i = from; i < to && mains < MAX_MAIN; i++) {
        const dock_session_t *s = &s_snap->sessions[i];
        if (i == from) {
            snprintf(s_first_id, sizeof(s_first_id), "%s", s->id);
        }
        if (mains > 0) {
            y += GROUP_GAP;
        }
        y += render_main(&ui.rows[mains++], s, y, now);
        for (int k = 0; k < s->agent_count && subs < MAX_SUB; k++) {
            y += SUB_GAP;
            render_sub(&ui.subs[subs++], &s->agents[k], y, now);
            y += SUB_H;
        }
    }
    for (int i = mains; i < MAX_MAIN; i++) {
        lv_obj_add_flag(ui.rows[i].obj, LV_OBJ_FLAG_HIDDEN);
    }
    for (int i = subs; i < MAX_SUB; i++) {
        lv_obj_add_flag(ui.subs[i].obj, LV_OBJ_FLAG_HIDDEN);
    }

    if (n == 0) {
        lv_obj_remove_flag(ui.empty, LV_OBJ_FLAG_HIDDEN);
    } else {
        lv_obj_add_flag(ui.empty, LV_OBJ_FLAG_HIDDEN);
    }

    if (s_page_count > 1) {
        lv_obj_remove_flag(ui.pager, LV_OBJ_FLAG_HIDDEN);
        for (int i = 0; i < MAX_DOTS; i++) {
            if (i >= s_page_count) {
                lv_obj_add_flag(ui.dots[i], LV_OBJ_FLAG_HIDDEN);
                continue;
            }
            lv_obj_remove_flag(ui.dots[i], LV_OBJ_FLAG_HIDDEN);
            bool on = i == s_page;
            lv_obj_set_height(ui.dots[i], on ? 20 : 6);
            lv_obj_set_style_bg_color(ui.dots[i], lv_color_hex(on ? 0xE9E9E5 : 0x3A3A3C), 0);
        }
    } else {
        lv_obj_add_flag(ui.pager, LV_OBJ_FLAG_HIDDEN);
    }
}

// What the waiting card says about WiFi: where go-dock reaches the screen,
// or why it cannot. False when WiFi is not set up.
static bool wifi_hint(const wlan_status_t *w, char *sub, size_t subn, char *note, size_t noten)
{
    switch (w->state) {
    case WLAN_CONNECTING:
        snprintf(sub, subn, "正在连接 WiFi「%s」…", w->ssid);
        snprintf(note, noten, "也可以用 USB 连接电脑");
        return true;
    case WLAN_CONNECTED:
        snprintf(sub, subn, "在 go-dock 里填屏幕地址 %s", w->ip);
        snprintf(note, noten, "配对码 %s", wlan_code());
        return true;
    case WLAN_FAILED:
        snprintf(sub, subn, "WiFi「%s」连不上：%s", w->ssid, w->err);
        snprintf(note, noten, "用 USB 连接电脑，在 go-dock 里重新设置 WiFi");
        return true;
    default:
        return false;
    }
}

// Shows why there is nothing current to display, or hides the card.
static void render_overlay(int64_t now_us)
{
    int64_t last = s_shown_rx_us;
    const char *title = NULL, *icon = LV_SYMBOL_USB;
    uint32_t icon_color = C_RED;
    char sub[160] = "", note[128] = "", ver[112];
    wlan_status_t wifi = wlan_status();
    int pct = -1; // progress bar, hidden when < 0
    int progress;
    const char *err;
    ota_state_t ota = ota_status(&progress, &err);

    bool asking = ota == OTA_ASKING;
    const char *keep = ota_over_wifi() ? "请勿断电" : "请勿拔线";
    snprintf(ver, sizeof(ver), "固件 %s", ota_version());
    if (asking) {
        // An update over WiFi waits for a yes on the screen.
        icon = LV_SYMBOL_DOWNLOAD;
        icon_color = C_GREEN;
        title = "升级固件？";
        snprintf(sub, sizeof(sub), "%s → %s", ota_version(), ota_target());
        const char *from = ota_from();
        snprintf(note, sizeof(note), "来自「%s」 · %d 秒后自动取消", from[0] ? from : "go-dock", ota_ask_left());
        ver[0] = '\0';
    } else if (ota != OTA_IDLE) {
        // An update outranks everything: the user must not unplug now.
        icon = LV_SYMBOL_DOWNLOAD;
        icon_color = C_GREEN;
        snprintf(ver, sizeof(ver), "固件 %s → %s", ota_version(), ota_target());
        if (ota == OTA_PREPARING) {
            title = "正在升级固件";
            snprintf(sub, sizeof(sub), "正在准备… %s", keep);
            pct = 0;
        } else if (ota == OTA_RECEIVING) {
            title = "正在升级固件";
            snprintf(sub, sizeof(sub), "%d%% · %s", progress, keep);
            pct = progress;
        } else if (ota == OTA_RESTARTING) {
            title = "升级完成";
            snprintf(sub, sizeof(sub), "正在重启…");
            pct = 100;
        } else {
            title = "固件升级失败";
            icon = LV_SYMBOL_WARNING;
            icon_color = C_RED;
            snprintf(sub, sizeof(sub), "%s", err);
            snprintf(note, sizeof(note), "仍在使用原来的固件");
            snprintf(ver, sizeof(ver), "固件 %s", ota_version());
        }
    } else if (s_info) {
        // The connection page says where to reach the screen itself.
    } else if (s_no_page && (last == 0 || now_us - last > OFFLINE_AFTER_US)) {
        title = "版本不兼容";
        snprintf(sub, sizeof(sub), "请把 go-dock 和固件升级到同一版本");
    } else if (last == 0) {
        title = "等待 go-dock";
        if (wifi_hint(&wifi, sub, sizeof(sub), note, sizeof(note))) {
            icon = LV_SYMBOL_WIFI;
        } else {
            // WiFi is set up from go-dock over the cable.
            snprintf(sub, sizeof(sub), "用 USB 连接电脑，在 go-dock 里设置屏幕 WiFi");
        }
    } else if (now_us - last > OFFLINE_AFTER_US) {
        title = "主机离线";
        icon = LV_SYMBOL_WIFI;
        int64_t sec = (now_us - last) / 1000000;
        const char *from = s_shown_name[0] ? s_shown_name : "go-dock";
        if (sec < 120) {
            snprintf(sub, sizeof(sub), "已 %d 秒没有收到「%s」的数据", (int)sec, from);
        } else {
            snprintf(sub, sizeof(sub), "已 %d 分钟没有收到「%s」的数据", (int)(sec / 60), from);
        }
        // A WiFi problem is the likely reason; else say how old the state is.
        if (wifi.state == WLAN_CONNECTING) {
            snprintf(note, sizeof(note), "正在重新连接 WiFi「%s」…", wifi.ssid);
        } else if (wifi.state == WLAN_FAILED) {
            snprintf(note, sizeof(note), "WiFi「%s」连不上：%s", wifi.ssid, wifi.err);
        } else if (s_have) {
            char hm[8];
            fmt_clock(hm, sizeof(hm), s_snap->now);
            snprintf(note, sizeof(note), "下面是 %s 的状态", hm);
        }
    }
    // Where go-dock reaches the screen, while it has nobody to show.
    if (title && ota == OTA_IDLE && last != 0 && wifi.state == WLAN_CONNECTED) {
        size_t n = strlen(ver);
        snprintf(ver + n, sizeof(ver) - n, " · 地址 %s · 配对码 %s", wifi.ip, wlan_code());
    }

    show(ui.ov_no, asking);
    show(ui.ov_yes, asking);
    if (!title) {
        lv_obj_add_flag(ui.overlay, LV_OBJ_FLAG_HIDDEN);
        lv_obj_set_style_opa(ui.content, LV_OPA_COVER, 0);
        lv_obj_set_style_bg_color(ui.link_dot, lv_color_hex(C_GREEN), 0);
        return;
    }
    // The question has its answers at the bottom: a taller card.
    lv_obj_set_height(ui.overlay, asking ? 240 : 204);
    lv_obj_center(ui.overlay);
    set_text(ui.ov_icon, icon);
    set_color(ui.ov_icon, icon_color);
    set_text(ui.ov_title, title);
    set_text(ui.ov_sub, sub);
    set_text(ui.ov_note, note);
    set_text(ui.ov_ver, ver);
    lv_obj_align(ui.ov_icon, LV_ALIGN_TOP_MID, 0, 18);
    lv_obj_align(ui.ov_title, LV_ALIGN_TOP_MID, 0, 54);
    lv_obj_align(ui.ov_sub, LV_ALIGN_TOP_MID, 0, 100);
    lv_obj_align(ui.ov_note, LV_ALIGN_TOP_MID, 0, 130);
    lv_obj_align(ui.ov_ver, LV_ALIGN_BOTTOM_MID, 0, -14);
    if (pct >= 0) {
        lv_bar_set_value(ui.ov_bar, pct, LV_ANIM_OFF);
        lv_obj_remove_flag(ui.ov_bar, LV_OBJ_FLAG_HIDDEN);
    } else {
        lv_obj_add_flag(ui.ov_bar, LV_OBJ_FLAG_HIDDEN);
    }
    lv_obj_remove_flag(ui.overlay, LV_OBJ_FLAG_HIDDEN);
    lv_obj_set_style_opa(ui.content, 90, 0); // about 35%
    lv_obj_set_style_bg_color(ui.link_dot, lv_color_hex(C_RED), 0);
}

static void render_clock(void)
{
    if (!s_have) {
        return;
    }
    char buf[8];
    fmt_clock(buf, sizeof(buf), host_now());
    set_text(ui.clock, buf);
    lv_obj_align_to(ui.link_dot, ui.clock, LV_ALIGN_OUT_LEFT_MID, -8, 0);
}

static void shift_pixels(int64_t now_us)
{
    static const int8_t offs[][2] = {{0, 0}, {2, 0}, {2, 2}, {0, 2}, {-2, 2}, {-2, 0}, {-2, -2}, {0, -2}, {2, -2}};
    if (now_us - s_shift_us < SHIFT_EVERY_US) {
        return;
    }
    s_shift_us = now_us;
    s_shift = (s_shift + 1) % (int)(sizeof(offs) / sizeof(offs[0]));
    lv_obj_set_pos(ui.root, offs[s_shift][0], offs[s_shift][1]);
}

void ui_touched(void)
{
    s_touch_us = esp_timer_get_time();
}

// show_host puts slot's page on screen, from its first task page; the
// snapshot and line come on the next take.
static void show_host(int slot, int64_t now_us)
{
    s_shown = slot;
    s_shown_gen = s_hosts[slot].gen;
    snprintf(s_shown_id, sizeof(s_shown_id), "%s", s_hosts[slot].id);
    snprintf(s_shown_name, sizeof(s_shown_name), "%s", s_hosts[slot].name);
    s_shown_at = s_seen_us[slot] = now_us;
    s_version = s_line_version = 0;
    s_page = 0;
    s_first_id[0] = '\0';
}

// The connection page's texts: the address and pairing code, or why there
// is no address; the computers on the screen; the WiFi and the firmware.
static void render_info(void)
{
    if (!s_info) {
        return;
    }
    wlan_status_t w = wlan_status();
    bool up = w.state == WLAN_CONNECTED;
    set_text(ui.info_addr, up ? w.ip : "—");
    set_text(ui.info_code, wlan_code());

    char buf[200];
    switch (w.state) {
    case WLAN_CONNECTED:
        snprintf(buf, sizeof(buf), "在另一台电脑的 go-dock「设置 → 设备」里填上这两项");
        break;
    case WLAN_CONNECTING:
        snprintf(buf, sizeof(buf), "正在连接 WiFi「%s」…", w.ssid);
        break;
    case WLAN_FAILED:
        snprintf(buf, sizeof(buf), "WiFi「%s」连不上：%s", w.ssid, w.err);
        break;
    default:
        snprintf(buf, sizeof(buf), "用 USB 连接电脑，在 go-dock 里设置屏幕 WiFi");
        break;
    }
    set_text(ui.info_hint, buf);

    size_t n = 0;
    if (s_order_n == 0) {
        n = snprintf(buf, sizeof(buf), "还没有电脑连着这块屏幕");
    } else {
        n = snprintf(buf, sizeof(buf), "已连接 %d 台：", s_order_n);
    }
    for (int i = 0; i < s_order_n && n < sizeof(buf); i++) {
        n += snprintf(buf + n, sizeof(buf) - n, "%s%s", i ? "、" : "", s_hosts[s_order[i]].name);
    }
    set_text(ui.info_hosts, buf);

    n = 0;
    if (w.ssid[0]) {
        n = snprintf(buf, sizeof(buf), "WiFi「%s」 · ", w.ssid);
    }
    if (up && n < sizeof(buf)) {
        n += snprintf(buf + n, sizeof(buf) - n, "%s · ", w.fixed ? "固定 IP" : "IP 由路由器分配，可能会变");
    }
    if (n < sizeof(buf)) {
        snprintf(buf + n, sizeof(buf) - n, "固件 %s", ota_version());
    }
    set_text(ui.info_wifi, buf);
}

// show_info puts the connection page on screen, noting who is on the
// screen then, or takes it off for the computer page under it.
static void show_info(bool on, int64_t now_us)
{
    if (on) {
        s_info_at = now_us;
        memset(s_info_ids, 0, sizeof(s_info_ids));
        for (int i = 0; i < s_order_n; i++) {
            snprintf(s_info_ids[i], sizeof(s_info_ids[i]), "%s", s_hosts[s_order[i]].id);
        }
    }
    s_info = on;
    show(ui.agent, !on);
    show(ui.info, on);
    render_info();
    render_strip();
}

// turn_host shows the next page (delta 1) or the one before (-1), in the
// order of the dots: the computers', then the connection page. With no
// computer the card in the middle says where to reach the screen.
static void turn_host(int delta)
{
    if (s_order_n == 0) {
        return;
    }
    int n = s_order_n + 1, at = s_order_n;
    for (int i = 0; !s_info && i < s_order_n; i++) {
        if (s_order[i] == s_shown) {
            at = i;
        }
    }
    int next = ((at + delta) % n + n) % n;
    int64_t now = esp_timer_get_time();
    if (next == s_order_n) {
        show_info(true, now);
        return;
    }
    if (s_info) {
        show_info(false, now);
    }
    show_host(s_order[next], now);
    render_strip();
}

// follow_hosts reads who is on the screen and turns to another page when
// hosts.c says so. A go-dock that came back on a new link keeps its page.
static void follow_hosts(int64_t now_us)
{
    link_hosts(s_hosts);
    for (int i = 0; i < LINK_HOSTS; i++) {
        if (s_seen_gen[i] != s_hosts[i].gen) {
            s_seen_gen[i] = s_hosts[i].gen;
            s_seen_us[i] = 0;
        }
    }
    if (s_shown >= 0 && s_hosts[s_shown].gen != s_shown_gen) {
        int same = -1;
        for (int i = 0; s_shown_id[0] && i < LINK_HOSTS; i++) {
            if (s_hosts[i].gen && strcmp(s_hosts[i].id, s_shown_id) == 0) {
                same = i;
            }
        }
        s_shown = same;
        if (same >= 0) {
            s_shown_gen = s_hosts[same].gen;
            s_version = s_line_version = 0;
        }
    }
    if (s_info) {
        // A computer that joined meanwhile shows that it did.
        int joined = -1;
        for (int i = 0; i < LINK_HOSTS; i++) {
            if (!host_present(&s_hosts[i], now_us)) {
                continue;
            }
            bool known = false;
            for (int k = 0; k < LINK_HOSTS; k++) {
                known = known || strcmp(s_info_ids[k], s_hosts[i].id) == 0;
            }
            if (!known) {
                joined = i;
            }
        }
        int64_t since = s_touch_us > s_info_at ? s_touch_us : s_info_at;
        if (joined >= 0 || now_us - since >= INFO_HOLD_US) {
            show_info(false, now_us);
        }
        if (joined >= 0) {
            show_host(joined, now_us);
        }
    }
    if (!s_info) {
        int pick = host_pick(s_hosts, s_seen_us, s_shown, s_shown_at, s_touch_us, now_us);
        if (pick >= 0 && pick != s_shown) {
            show_host(pick, now_us);
        }
    }

    s_order_n = 0;
    s_attention = s_no_page = false;
    for (int i = 0; i < LINK_HOSTS; i++) {
        const link_host_t *h = &s_hosts[i];
        s_no_page = s_no_page || (h->gen && h->incompatible);
        if (!host_present(h, now_us)) {
            continue;
        }
        int k = s_order_n++;
        for (; k > 0 && strcmp(s_hosts[s_order[k - 1]].id, h->id) > 0; k--) {
            s_order[k] = s_order[k - 1];
        }
        s_order[k] = i;
        s_attention = s_attention || h->attention;
        if (h->active) {
            s_active_us = now_us;
        }
    }
    s_no_page = s_no_page && s_order_n == 0;
    if (s_shown >= 0) {
        const link_host_t *h = &s_hosts[s_shown];
        if (!s_info) {
            s_seen_us[s_shown] = now_us;
        }
        s_shown_rx_us = h->last_rx_us;
        snprintf(s_shown_name, sizeof(s_shown_name), "%s", h->name);
    }
}

void ui_poll(void)
{
    int64_t now_us = esp_timer_get_time();
    follow_hosts(now_us);
    bool fresh = s_shown >= 0 && link_take_snapshot(s_shown, s_shown_gen, s_snap, &s_version, &s_rx_us);
    if (fresh) {
        s_have = true;
        // Stay on the page that showed the same first task, if it is still there.
        int keep = -1;
        for (int i = 0; s_first_id[0] && i < s_snap->count; i++) {
            if (strcmp(s_snap->sessions[i].id, s_first_id) == 0) {
                keep = i;
                break;
            }
        }
        layout_pages();
        s_page = keep >= 0 ? page_of(keep) : (s_first_id[0] ? 0 : s_page);
        render_strip();
        render_rows();
    }
    take_line(now_us);
    tick_line(now_us);
    // An update's progress shows at once, not on the next second.
    static ota_state_t ota_shown;
    static int pct_shown;
    int pct;
    ota_state_t ota = ota_status(&pct, NULL);
    if (ota != ota_shown || pct != pct_shown) {
        ota_shown = ota;
        pct_shown = pct;
        render_overlay(now_us);
    }
    if (fresh || now_us - s_last_second_us >= 1000000) {
        s_last_second_us = now_us;
        int64_t now = host_now();
        render_usage(now);
        render_clock();
        render_strip(); // the computers' dots
        render_info();
        render_overlay(now_us);
        if (!fresh) {
            render_rows(); // elapsed times
        }
        shift_pixels(now_us);
    }
}

static uint8_t brightness(void)
{
    settings_t st = settings_get();
    uint8_t full = full_level();
    int64_t now_us = esp_timer_get_time();
    int64_t last = s_shown_rx_us;
    if (ota_status(NULL, NULL) != OTA_IDLE || s_info) {
        return full; // an update, or someone reading the address
    }
    if (last == 0 || now_us - last > OFFLINE_AFTER_US) {
        bool long_gone = last == 0 ? now_us > 10 * 60 * 1000000LL : now_us - last > 10 * 60 * 1000000LL;
        uint8_t away = long_gone ? BRIGHT_ASLEEP : BRIGHT_OFFLINE;
        return now_us - s_touch_us < TOUCH_WAKE_US || away > full ? full : away;
    }
    if (s_attention) {
        return full; // a computer needs the user: never dim
    }
    if (st.dim_after == 0 || now_us - s_touch_us < TOUCH_WAKE_US || now_us - s_active_us < st.dim_after * 1000000LL) {
        return full;
    }
    uint8_t dim = (uint8_t)(st.dim_brightness * 255 / 100);
    return dim < full ? dim : full;
}

uint8_t ui_brightness(void)
{
    return brightness();
}
