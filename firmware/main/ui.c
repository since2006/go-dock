#include "ui.h"

#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <time.h>

#include "esp_heap_caps.h"
#include "esp_timer.h"
#include "fonts.h"
#include "link.h"
#include "lvgl.h"
#include "model.h"
#include "ota.h"
#include "sdkconfig.h"

#define SCR_W 600
#define SCR_H 450
#define PAD_X 18
#define TOP 14
#define TASKS_Y 178
#define TASKS_H 258
#define LIST_W 540
#define MAX_DOTS 8

#define OFFLINE_AFTER_US (15 * 1000000LL)
#define DIM_AFTER_US (5 * 60 * 1000000LL)  // no active task for this long
#define TOUCH_WAKE_US (60 * 1000000LL)
#define SHIFT_EVERY_US (60 * 1000000LL)    // pixel shift against burn-in

#define BRIGHT_FULL 200
#define BRIGHT_DIM 50
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
#define C_BLUE 0x5AA9FF
#define C_LAMP 0xFFC14D

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
// §7.1): the project line carries the session name on the left and the
// state, time and turn tokens on the right; the lines below span the full
// width. A session row is 80 px with the assistant's latest text and an
// action line, 60 px with one or none (then the project line is centred);
// each subagent adds a 32 px row under it; a session and its subagents
// never split across pages.
#define MAIN_H 60
#define MAIN_H_TALL 80
#define SUB_H 32
#define SUB_GAP 4
#define GROUP_GAP 6
#define SUB_X 52
#define MAX_MAIN 4 // session rows that fit on a page
#define MAX_SUB 6  // subagent rows that fit on a page
#define MAX_PAGES 32
#define CHIP 40
#define MID_X (12 + CHIP + 12)
#define END_X (LIST_W - 14)     // right edge of the row's text
#define LINE_W (END_X - MID_X) // lines under the project line

// Function pages, shared with the web preview (§7.1): the items as cards in
// a grid under the strip, one row up to three, then 2x2 and 3x2.
#define FN_Y 54
#define FN_W (SCR_W - 2 * PAD_X)
#define FN_H 380
#define CARD_GAP 12
#define MAX_FN (1 + DOCK_MAX_PAGES) // the Agent page, then the function pages

typedef struct {
    lv_obj_t *reset, *window, *pct, *bar;
} meter_t;

typedef struct {
    lv_obj_t *obj, *chip, *icon, *spinner, *project, *name, *prov, *label, *time, *tok, *msg,
        *detail;
} row_t;

typedef struct {
    lv_obj_t *obj, *icon, *spinner, *type, *text, *label, *time;
} sub_t;

typedef struct {
    lv_obj_t *obj, *chip, *label, *text;
    lv_obj_t *steps, *dec, *value, *inc; // a stepper's − value +
} card_t;

static struct {
    lv_obj_t *root;    // shifted a few px now and then against burn-in
    lv_obj_t *content; // dimmed when the host is away
    lv_obj_t *title, *summary, *link_dot, *clock;
    lv_obj_t *fn_pager, *fn_dots[MAX_FN]; // which page is shown
    lv_obj_t *agent;                      // the Agent page's body
    lv_obj_t *fn, *fn_empty;              // a function page's body
    card_t cards[DOCK_MAX_ITEMS];
    struct {
        meter_t m[2];
        lv_obj_t *today;
    } usage[2];
    row_t rows[MAX_MAIN];
    sub_t subs[MAX_SUB];
    lv_obj_t *pager, *dots[MAX_DOTS];
    lv_obj_t *empty;
    lv_obj_t *overlay, *ov_icon, *ov_title, *ov_sub, *ov_note, *ov_bar, *ov_ver;
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

static dock_pages_t *s_pages;
static uint32_t s_pages_version;
static int s_fn;                       // page shown: 0 Agent, i the function page s_pages->pages[i - 1]
static char s_fn_id[DOCK_ID_LEN];      // its id, to stay on it when the pages change
static bool s_wake_tap;                // the press under way woke a dim screen: no tap
static uint8_t s_bright = BRIGHT_FULL; // as last asked for, before any new touch

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

static bool is_tall(const dock_session_t *s)
{
    return s->msg[0] && s->detail[0];
}

// The one line under the project line of a short row: msg or detail,
// whichever there is; older hosts only send text, which may repeat the
// session name. "" leaves the project line alone.
static const char *only_line(const dock_session_t *s)
{
    if (s->msg[0]) {
        return s->msg;
    }
    if (s->detail[0]) {
        return s->detail;
    }
    return strcmp(s->text, s->title) ? s->text : "";
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
        int h = (is_tall(s) ? MAIN_H_TALL : MAIN_H) + s->agent_count * (SUB_H + SUB_GAP);
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

// The strip over every page: its name and summary on the left; page dots,
// the link dot and the clock on the right.
static void build_strip(lv_obj_t *p)
{
    ui.title = text(p, font_get(FONT_BOLD, 20), C_TEXT);
    lv_label_set_text(ui.title, "Agent");
    lv_obj_set_pos(ui.title, PAD_X, TOP);

    ui.summary = line(p, font_get(FONT_REGULAR, 15), C_TEXT2, 360);
    lv_obj_align_to(ui.summary, ui.title, LV_ALIGN_OUT_RIGHT_BOTTOM, 12, -3);

    ui.clock = text(p, font_get(FONT_MONO, 20), C_TEXT);
    lv_label_set_text(ui.clock, "--:--");
    lv_obj_align(ui.clock, LV_ALIGN_TOP_RIGHT, -PAD_X, TOP + 2);

    ui.link_dot = box(p, 0, 0, 8, 8, C_GREEN, LV_RADIUS_CIRCLE);
    lv_obj_align_to(ui.link_dot, ui.clock, LV_ALIGN_OUT_LEFT_MID, -8, 0);

    ui.fn_pager = box(p, 0, 0, LV_SIZE_CONTENT, 6, 0, -1);
    lv_obj_set_flex_flow(ui.fn_pager, LV_FLEX_FLOW_ROW);
    lv_obj_set_flex_align(ui.fn_pager, LV_FLEX_ALIGN_START, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_column(ui.fn_pager, 5, 0);
    for (int i = 0; i < MAX_FN; i++) {
        ui.fn_dots[i] = box(ui.fn_pager, 0, 0, 6, 6, 0x3A3A3C, 3);
    }
}

// "4h 33m / 5h" on the left, the percentage on the right, the bar below.
static void build_meter(lv_obj_t *row, meter_t *m, int x, const char *name)
{
    const int w = 182;
    m->reset = text(row, font_get(FONT_MONO, 13), C_TEXT2);
    lv_obj_set_pos(m->reset, x, 7);

    m->window = text(row, font_get(FONT_MONO, 13), C_TEXT5);
    char buf[16];
    snprintf(buf, sizeof(buf), " / %s", name);
    lv_label_set_text(m->window, buf);

    m->pct = text(row, font_get(FONT_MONO_BOLD, 21), C_TEXT);
    lv_obj_set_width(m->pct, 80);
    lv_obj_set_style_text_align(m->pct, LV_TEXT_ALIGN_RIGHT, 0);
    lv_obj_set_pos(m->pct, x + w - 80, 0);

    m->bar = lv_bar_create(row);
    lv_obj_remove_style_all(m->bar);
    lv_obj_set_pos(m->bar, x, 32);
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
    lv_obj_t *card = box(p, PAD_X, 54, SCR_W - 2 * PAD_X, 114, C_CARD, 18);
    for (int i = 0; i < 2; i++) {
        lv_obj_t *row = box(card, 16, 10 + i * 50, 532, 44, 0, -1);
        lv_obj_t *name = text(row, font_get(FONT_BOLD, 19), PROVIDER_COLOR[i]);
        lv_label_set_text(name, PROVIDER_NAME[i]);
        lv_obj_align(name, LV_ALIGN_LEFT_MID, 0, 0);
        build_meter(row, &ui.usage[i].m[0], 84, "5h");
        build_meter(row, &ui.usage[i].m[1], 280, "Weekly");

        ui.usage[i].today = text(row, font_get(FONT_MONO, 17), C_TEXT);
        lv_obj_set_width(ui.usage[i].today, 58);
        lv_obj_set_style_text_align(ui.usage[i].today, LV_TEXT_ALIGN_RIGHT, 0);
        lv_obj_set_pos(ui.usage[i].today, 474, 0);
        lv_obj_t *l = text(row, font_get(FONT_REGULAR, 13), C_TEXT4);
        lv_label_set_text(l, "今日");
        lv_obj_align(l, LV_ALIGN_BOTTOM_RIGHT, 0, 0);
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

// Session row; positions depend on its height and are set when rendered.
static void build_row(lv_obj_t *p, row_t *r)
{
    r->obj = box(p, 0, 0, LIST_W, MAIN_H, C_CARD, 14);
    lv_obj_set_style_border_width(r->obj, 2, 0);
    lv_obj_set_style_border_opa(r->obj, LV_OPA_TRANSP, 0);

    r->chip = box(r->obj, 12, 0, CHIP, CHIP, 0, LV_RADIUS_CIRCLE);
    lv_obj_set_style_bg_opa(r->chip, 41, 0); // ~16% of the state color
    r->icon = text(r->chip, &lv_font_montserrat_20, C_TEXT);
    lv_obj_center(r->icon);
    r->spinner = spinner(r->chip, 22, 3);
    lv_obj_center(r->spinner);

    const lv_font_t *fp = font_get(FONT_BOLD, 22);
    r->project = text(r->obj, fp, C_TEXT);
    lv_label_set_long_mode(r->project, LV_LABEL_LONG_DOT);
    lv_obj_set_height(r->project, lv_font_get_line_height(fp)); // one line, "..." when cut
    const lv_font_t *fn = font_get(FONT_REGULAR, 14);
    r->name = text(r->obj, fn, C_TEXT4);
    lv_label_set_long_mode(r->name, LV_LABEL_LONG_DOT);
    lv_obj_set_height(r->name, lv_font_get_line_height(fn));
    r->prov = text(r->obj, font_get(FONT_BOLD, 13), C_TEXT);

    // State, time and " ↓6.5K" at the right end of the project line.
    r->label = text(r->obj, font_get(FONT_BOLD, 18), C_TEXT);
    r->time = text(r->obj, font_get(FONT_MONO, 15), C_TEXT3);
    r->tok = text(r->obj, font_get(FONT_MONO, 15), C_TEXT5);

    r->msg = line(r->obj, font_get(FONT_REGULAR, 15), 0xC9C9C4, LINE_W);
    r->detail = line(r->obj, font_get(FONT_REGULAR, 16), C_TEXT2, LINE_W);
}

// Subagent row, indented under its session.
static void build_sub(lv_obj_t *p, sub_t *u)
{
    const int w = LIST_W - SUB_X;
    u->obj = box(p, SUB_X, 0, w, SUB_H, 0x0B0B0C, 10);
    u->icon = text(u->obj, &lv_font_montserrat_12, C_TEXT);
    lv_obj_align(u->icon, LV_ALIGN_LEFT_MID, 11, 0);
    u->spinner = spinner(u->obj, 14, 2);
    lv_obj_align(u->spinner, LV_ALIGN_LEFT_MID, 9, 0);
    u->type = text(u->obj, font_get(FONT_BOLD, 15), C_TEXT);
    lv_obj_align(u->type, LV_ALIGN_LEFT_MID, 32, 0);
    u->text = line(u->obj, font_get(FONT_REGULAR, 14), C_TEXT2, 200);
    u->time = text(u->obj, font_get(FONT_MONO, 13), C_TEXT3);
    lv_obj_set_width(u->time, 56);
    lv_obj_set_style_text_align(u->time, LV_TEXT_ALIGN_RIGHT, 0);
    lv_obj_align(u->time, LV_ALIGN_RIGHT_MID, -12, 0);
    u->label = text(u->obj, font_get(FONT_BOLD, 14), C_TEXT);
    lv_obj_set_width(u->label, 64);
    lv_obj_set_style_text_align(u->label, LV_TEXT_ALIGN_RIGHT, 0);
    lv_obj_align(u->label, LV_ALIGN_RIGHT_MID, -12 - 56 - 6, 0);
}

static void go_page(int delta)
{
    int n = s_page_count;
    s_page = ((s_page + delta) % n + n) % n;
    s_first_id[0] = '\0'; // keep the page the user chose
}

static void render_rows(void);
static void render_page(void);
static void render_fn(void);

static void on_pager(lv_event_t *e)
{
    (void)e;
    go_page(1);
    render_rows();
}

// Shows page i: 0 is the Agent page, then the function pages in order.
static void show_fn(int i)
{
    s_fn = i;
    snprintf(s_fn_id, sizeof(s_fn_id), "%s", i ? s_pages->pages[i - 1].id : "");
    render_page();
}

static dock_page_t *fn_page(void)
{
    return s_fn > 0 && s_fn <= s_pages->count ? &s_pages->pages[s_fn - 1] : NULL;
}

static void on_gesture(lv_event_t *e)
{
    (void)e;
    lv_indev_t *indev = lv_indev_active();
    lv_dir_t dir = lv_indev_get_gesture_dir(indev);
    lv_indev_wait_release(indev); // a swipe never also taps what it started on
    // Left/right switches pages, stopping at both ends; up/down pages
    // through the Agent page's tasks.
    if (dir == LV_DIR_LEFT && s_fn < s_pages->count) {
        show_fn(s_fn + 1);
    } else if (dir == LV_DIR_RIGHT && s_fn > 0) {
        show_fn(s_fn - 1);
    } else if (s_fn == 0 && (dir == LV_DIR_TOP || dir == LV_DIR_BOTTOM)) {
        go_page(dir == LV_DIR_TOP ? 1 : -1);
        render_rows();
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
    lv_obj_t *t = text(ui.empty, font_get(FONT_BOLD, 24), C_TEXT);
    lv_label_set_text(t, "没有进行中的任务");
    lv_obj_align(t, LV_ALIGN_CENTER, 0, -14);
    lv_obj_t *s = text(ui.empty, font_get(FONT_REGULAR, 16), C_TEXT3);
    lv_label_set_text(s, "Claude 和 Codex 都在待命");
    lv_obj_align(s, LV_ALIGN_CENTER, 0, 22);

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

// The card over the dimmed screen: why there is nothing current to show,
// or a firmware update's progress; the firmware version at the bottom.
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
}

// Card icons, as the web preview's SCR_ICONS draws them on a 24-unit grid
// (coordinates here in tenths), with straight strokes and arcs only.
typedef struct {
    bool arc;
    int16_t v[5]; // line: x1 y1 x2 y2; arc: centre x y, radius, start and end angle (clockwise from 3 o'clock)
} stroke_t;

#define LINE(x1, y1, x2, y2) {false, {x1, y1, x2, y2, 0}}
#define ARC(cx, cy, r, a0, a1) {true, {cx, cy, r, a0, a1}}

static const stroke_t ICON_BULB[] = {
    ARC(120, 90, 60, 127, 53), // the head, open at the bottom for the neck
    LINE(84, 138, 95, 160), LINE(156, 138, 145, 160), LINE(95, 160, 145, 160),
    LINE(90, 180, 150, 180), LINE(100, 210, 140, 210),
};
static const stroke_t ICON_POWER[] = {LINE(120, 30, 120, 110), ARC(120, 120, 80, 315, 225)};
static const stroke_t ICON_COOL[] = {
    LINE(120, 25, 120, 215), LINE(38, 73, 202, 167), LINE(38, 167, 202, 73),
    LINE(95, 40, 120, 60), LINE(120, 60, 145, 40), LINE(95, 200, 120, 180), LINE(120, 180, 145, 200),
};
static const stroke_t ICON_HEAT[] = {
    ARC(120, 120, 40, 0, 360),
    LINE(120, 25, 120, 45), LINE(120, 195, 120, 215), LINE(25, 120, 45, 120), LINE(195, 120, 215, 120),
    LINE(53, 53, 67, 67), LINE(173, 173, 187, 187), LINE(53, 187, 67, 173), LINE(173, 67, 187, 53),
};
static const stroke_t ICON_DRY[] = {LINE(120, 30, 68, 115), LINE(172, 115, 120, 30), ARC(120, 145, 60, 330, 210)};
static const stroke_t ICON_FAN[] = {
    LINE(30, 80, 120, 80), ARC(120, 55, 25, 180, 90),
    LINE(30, 120, 180, 120), ARC(180, 145, 25, 270, 180),
    LINE(30, 160, 100, 160), ARC(100, 180, 20, 270, 180),
};
static const stroke_t ICON_AUTO[] = {
    ARC(120, 120, 80, 0, 138), ARC(120, 120, 80, 180, 318),
    LINE(180, 30, 180, 67), LINE(180, 67, 143, 67), LINE(60, 210, 60, 173), LINE(60, 173, 97, 173),
};

#define STROKES(a) {a, sizeof(a) / sizeof(a[0])}
static const struct {
    const stroke_t *s;
    int n;
} ICONS[DOCK_ICON_COUNT] = {
    [DOCK_ICON_BULB] = STROKES(ICON_BULB), [DOCK_ICON_POWER] = STROKES(ICON_POWER),
    [DOCK_ICON_COOL] = STROKES(ICON_COOL), [DOCK_ICON_HEAT] = STROKES(ICON_HEAT),
    [DOCK_ICON_DRY] = STROKES(ICON_DRY),   [DOCK_ICON_FAN] = STROKES(ICON_FAN),
    [DOCK_ICON_AUTO] = STROKES(ICON_AUTO),
};

// A select card shows its option in the colour of its icon.
static const uint32_t ICON_COLOR[DOCK_ICON_COUNT] = {
    [DOCK_ICON_BULB] = 0xE9E9E5, [DOCK_ICON_POWER] = 0xE9E9E5, [DOCK_ICON_COOL] = 0x5AA9FF, [DOCK_ICON_HEAT] = 0xFF9F43,
    [DOCK_ICON_DRY] = 0x4FD1C5,  [DOCK_ICON_FAN] = 0xE9E9E5,   [DOCK_ICON_AUTO] = 0x3DD68C,
};

// Draws the chip's icon (its user data) in the chip's arc and line
// colours: 46 px in a 96 px chip, 30 px in the 56 px one.
static void draw_icon(lv_event_t *e)
{
    lv_obj_t *chip = lv_event_get_current_target_obj(e);
    lv_layer_t *layer = lv_event_get_layer(e);
    dock_icon_t icon = (dock_icon_t)(intptr_t)lv_obj_get_user_data(chip);
    lv_area_t a;
    lv_obj_get_coords(chip, &a);
    int size = lv_area_get_width(&a) >= 96 ? 46 : 30;
    int w = (size * 2 + 12) / 24; // stroke 2 of 24
    int32_t ox = a.x1 + (lv_area_get_width(&a) - size) / 2;
    int32_t oy = a.y1 + (lv_area_get_height(&a) - size) / 2;

    lv_draw_arc_dsc_t arc;
    lv_draw_arc_dsc_init(&arc);
    arc.base.layer = layer;
    lv_obj_init_draw_arc_dsc(chip, LV_PART_MAIN, &arc);
    arc.width = w;
    arc.rounded = 1;
    lv_draw_line_dsc_t ln;
    lv_draw_line_dsc_init(&ln);
    ln.base.layer = layer;
    lv_obj_init_draw_line_dsc(chip, LV_PART_MAIN, &ln);
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

// The − or + on a stepper's button (user data 1 for +), in its line style.
static void draw_step(lv_event_t *e)
{
    lv_obj_t *b = lv_event_get_current_target_obj(e);
    lv_layer_t *layer = lv_event_get_layer(e);
    lv_area_t a;
    lv_obj_get_coords(b, &a);
    int32_t cx = (a.x1 + a.x2) / 2, cy = (a.y1 + a.y2) / 2, r = lv_area_get_width(&a) / 6;
    lv_draw_line_dsc_t ln;
    lv_draw_line_dsc_init(&ln);
    ln.base.layer = layer;
    lv_obj_init_draw_line_dsc(b, LV_PART_MAIN, &ln);
    ln.round_start = ln.round_end = 1;
    ln.p1.x = cx - r, ln.p1.y = cy, ln.p2.x = cx + r, ln.p2.y = cy;
    lv_draw_line(layer, &ln);
    if (lv_obj_get_user_data(b)) {
        ln.p1.x = cx, ln.p1.y = cy - r, ln.p2.x = cx, ln.p2.y = cy + r;
        lv_draw_line(layer, &ln);
    }
}

static void on_card_pressed(lv_event_t *e)
{
    (void)e;
    s_wake_tap = s_bright < BRIGHT_FULL; // a dim screen only wakes up
}

// The item tapped, if a tap may go to the host now.
static dock_item_t *tapped(dock_page_t *pg, int i)
{
    if (s_wake_tap || !pg || i >= pg->item_count || !lv_obj_has_flag(ui.overlay, LV_OBJ_FLAG_HIDDEN)) {
        return NULL;
    }
    dock_item_t *it = &pg->items[i];
    return it->disabled ? NULL : it;
}

// A tap on a card: a switch toggles and shows it is switching until the
// host's next pages; a select takes its next option, which the next pages
// show.
static void on_card_clicked(lv_event_t *e)
{
    dock_page_t *pg = fn_page();
    dock_item_t *it = tapped(pg, (int)(intptr_t)lv_event_get_user_data(e));
    if (!it) {
        return;
    }
    if (it->kind == DOCK_ITEM_SELECT) {
        link_send_action(pg->id, it->id, "next");
    } else if (it->kind == DOCK_ITEM_SWITCH) {
        link_send_action(pg->id, it->id, "toggle");
        it->busy = true;
        snprintf(it->text, sizeof(it->text), "切换中…");
        render_fn();
    }
}

// A tap on a stepper's − or + (user data: card index * 2, plus 1 for +).
static void on_step_clicked(lv_event_t *e)
{
    int v = (int)(intptr_t)lv_event_get_user_data(e);
    bool inc = v & 1;
    dock_page_t *pg = fn_page();
    dock_item_t *it = tapped(pg, v / 2);
    if (it && it->kind == DOCK_ITEM_STEPPER && !(inc ? it->at_max : it->at_min)) {
        link_send_action(pg->id, it->id, inc ? "inc" : "dec");
    }
}

static lv_obj_t *build_step(lv_obj_t *row, int index, bool inc)
{
    lv_obj_t *b = box(row, 0, 0, 56, 56, 0x1C1C1E, LV_RADIUS_CIRCLE);
    lv_obj_set_style_line_color(b, lv_color_hex(C_TEXT), 0);
    lv_obj_set_style_line_width(b, 3, 0);
    lv_obj_set_style_opa(b, 204, LV_STATE_PRESSED);
    lv_obj_set_user_data(b, (void *)(intptr_t)inc);
    lv_obj_add_event_cb(b, draw_step, LV_EVENT_DRAW_MAIN_END, NULL);
    lv_obj_add_event_cb(b, on_card_pressed, LV_EVENT_PRESSED, NULL);
    lv_obj_add_event_cb(b, on_step_clicked, LV_EVENT_CLICKED, (void *)(intptr_t)(index * 2 + inc));
    return b;
}

// Card: an icon in a round chip (or, for a stepper, − value +), the label
// and the state text, centred; sizes depend on the grid and are set when
// rendered.
static void build_card(lv_obj_t *p, card_t *c, int index)
{
    c->obj = box(p, 0, 0, 180, 184, C_CARD, 18);
    lv_obj_set_style_border_width(c->obj, 2, 0);
    lv_obj_set_style_border_color(c->obj, lv_color_hex(C_LAMP), 0);
    lv_obj_set_style_border_opa(c->obj, LV_OPA_TRANSP, 0);
    lv_obj_set_style_pad_all(c->obj, 12, 0);
    lv_obj_set_style_pad_row(c->obj, 8, 0);
    lv_obj_set_flex_flow(c->obj, LV_FLEX_FLOW_COLUMN);
    lv_obj_set_flex_align(c->obj, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_opa(c->obj, 204, LV_STATE_PRESSED);
    lv_obj_add_event_cb(c->obj, on_card_pressed, LV_EVENT_PRESSED, NULL);
    lv_obj_add_event_cb(c->obj, on_card_clicked, LV_EVENT_CLICKED, (void *)(intptr_t)index);

    c->chip = box(c->obj, 0, 0, 56, 56, 0x1C1C1E, LV_RADIUS_CIRCLE);
    lv_obj_remove_flag(c->chip, LV_OBJ_FLAG_CLICKABLE);
    lv_obj_set_style_arc_width(c->chip, 1, 0); // icons are drawn in these styles
    lv_obj_set_style_line_width(c->chip, 1, 0);
    lv_obj_add_event_cb(c->chip, draw_icon, LV_EVENT_DRAW_MAIN_END, NULL);

    c->steps = box(c->obj, 0, 0, LV_SIZE_CONTENT, LV_SIZE_CONTENT, 0, -1);
    lv_obj_remove_flag(c->steps, LV_OBJ_FLAG_CLICKABLE);
    lv_obj_set_flex_flow(c->steps, LV_FLEX_FLOW_ROW);
    lv_obj_set_flex_align(c->steps, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER, LV_FLEX_ALIGN_CENTER);
    lv_obj_set_style_pad_column(c->steps, 12, 0);
    c->dec = build_step(c->steps, index, false);
    c->value = text(c->steps, font_get(FONT_MONO_BOLD, 36), C_TEXT);
    lv_obj_set_style_text_align(c->value, LV_TEXT_ALIGN_CENTER, 0);
    c->inc = build_step(c->steps, index, true);

    c->label = line(c->obj, font_get(FONT_BOLD, 20), C_TEXT, 156);
    lv_obj_set_style_text_align(c->label, LV_TEXT_ALIGN_CENTER, 0);
    c->text = text(c->obj, font_get(FONT_BOLD, 16), C_TEXT3);
}

static void build_fn(lv_obj_t *p)
{
    ui.fn = box(p, PAD_X, FN_Y, FN_W, FN_H, 0, -1);
    for (int i = 0; i < DOCK_MAX_ITEMS; i++) {
        build_card(ui.fn, &ui.cards[i], i);
    }
    ui.fn_empty = box(ui.fn, 0, 0, FN_W, FN_H, 0x0B0B0C, 18);
    lv_obj_t *t = text(ui.fn_empty, font_get(FONT_REGULAR, 16), C_TEXT3);
    lv_label_set_text(t, "这一页还没有内容");
    lv_obj_center(t);
}

void ui_create(void)
{
    s_snap = heap_caps_calloc(1, sizeof(dock_snapshot_t), MALLOC_CAP_SPIRAM);
    s_pages = heap_caps_calloc(1, sizeof(dock_pages_t), MALLOC_CAP_SPIRAM);

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
    build_usage(ui.agent);
    build_tasks(ui.agent);
    build_fn(ui.content);
    build_overlay(ui.root);

    int64_t now = esp_timer_get_time();
    s_touch_us = s_active_us = s_shift_us = now;
    render_rows();
    render_page();
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

// The strip for the page shown: its name and summary, or in red why the
// page cannot be used now; page dots once there are function pages.
static void render_strip(void)
{
    const dock_page_t *pg = fn_page();
    char buf[100];
    if (pg) {
        snprintf(buf, sizeof(buf), "%s", pg->error[0] ? pg->error : pg->summary);
    } else {
        agent_summary(buf, sizeof(buf));
    }
    set_text(ui.title, pg ? pg->title : "Agent");
    set_text(ui.summary, buf);
    set_color(ui.summary, pg && pg->error[0] ? C_RED : C_TEXT2);

    int n = 1 + s_pages->count;
    for (int i = 0; i < MAX_FN; i++) {
        if (i >= n) {
            lv_obj_add_flag(ui.fn_dots[i], LV_OBJ_FLAG_HIDDEN);
            continue;
        }
        lv_obj_remove_flag(ui.fn_dots[i], LV_OBJ_FLAG_HIDDEN);
        lv_obj_set_width(ui.fn_dots[i], i == s_fn ? 18 : 6);
        lv_obj_set_style_bg_color(ui.fn_dots[i], lv_color_hex(i == s_fn ? 0xE9E9E5 : 0x3A3A3C), 0);
    }
    if (n > 1) {
        lv_obj_remove_flag(ui.fn_pager, LV_OBJ_FLAG_HIDDEN);
    } else {
        lv_obj_add_flag(ui.fn_pager, LV_OBJ_FLAG_HIDDEN);
    }
    lv_obj_update_layout(ui.content);
    lv_obj_align_to(ui.fn_pager, ui.link_dot, LV_ALIGN_OUT_LEFT_MID, -16, 0);
    // The summary takes the room between the title and the dots.
    lv_obj_align_to(ui.summary, ui.title, LV_ALIGN_OUT_RIGHT_BOTTOM, 12, -3);
    int end = (n > 1 ? lv_obj_get_x(ui.fn_pager) : lv_obj_get_x(ui.link_dot)) - 12;
    int w = end - lv_obj_get_x(ui.summary);
    lv_obj_set_width(ui.summary, w > 1 ? w : 1);
}

static void set_opa(void *obj, int32_t v)
{
    lv_obj_set_style_opa(obj, (lv_opa_t)v, 0);
}

// Pulses the icon of a card that is switching, about once a second.
static void pulse(lv_obj_t *chip, bool on)
{
    bool running = lv_anim_get(chip, set_opa) != NULL;
    if (on == running) {
        return;
    }
    if (!on) {
        lv_anim_delete(chip, set_opa);
        lv_obj_set_style_opa(chip, LV_OPA_COVER, 0);
        return;
    }
    lv_anim_t a;
    lv_anim_init(&a);
    lv_anim_set_var(&a, chip);
    lv_anim_set_exec_cb(&a, set_opa);
    lv_anim_set_values(&a, LV_OPA_COVER, 115); // down to 45%
    lv_anim_set_duration(&a, 500);
    lv_anim_set_reverse_duration(&a, 500);
    lv_anim_set_path_cb(&a, lv_anim_path_ease_in_out);
    lv_anim_set_repeat_count(&a, LV_ANIM_REPEAT_INFINITE);
    lv_anim_start(&a);
}

static void show(lv_obj_t *o, bool on)
{
    if (on) {
        lv_obj_remove_flag(o, LV_OBJ_FLAG_HIDDEN);
    } else {
        lv_obj_add_flag(o, LV_OBJ_FLAG_HIDDEN);
    }
}

static void set_tappable(lv_obj_t *o, bool on)
{
    if (on) {
        lv_obj_add_flag(o, LV_OBJ_FLAG_CLICKABLE);
    } else {
        lv_obj_remove_flag(o, LV_OBJ_FLAG_CLICKABLE);
    }
}

// A stepper's − or +: greyed and deaf at the end of the range.
static void render_step(lv_obj_t *b, bool big, bool usable)
{
    int d = big ? 72 : 56;
    lv_obj_set_size(b, d, d);
    lv_obj_set_style_line_width(b, big ? 4 : 3, 0);
    lv_obj_set_style_opa(b, usable ? LV_OPA_COVER : 77, 0); // 30%
    set_tappable(b, usable);
}

// Lays out card c for item it at x, y; a single row of cards gets the
// large icon and type. A switch shows on / off (lit cards keep to a dark
// tint and a thin ring, kind to the AMOLED panel), a select its option in
// the colour of its icon, a stepper − value +.
static void render_card(card_t *c, const dock_item_t *it, int x, int y, int w, int h, bool big)
{
    bool sw = it->kind == DOCK_ITEM_SWITCH, step = it->kind == DOCK_ITEM_STEPPER;
    bool lit = sw && !it->busy && it->on == 1;
    uint32_t color = it->busy ? C_BLUE
                     : sw     ? (it->on == 1 ? C_LAMP : it->on == 0 ? C_TEXT3 : 0x5C5B57)
                              : ICON_COLOR[it->icon];

    lv_obj_remove_flag(c->obj, LV_OBJ_FLAG_HIDDEN);
    lv_obj_set_pos(c->obj, x, y);
    lv_obj_set_size(c->obj, w, h);
    lv_obj_set_style_bg_color(c->obj, lv_color_hex(lit ? 0x1E1606 : C_CARD), 0);
    lv_obj_set_style_border_opa(c->obj, lit ? 102 : LV_OPA_TRANSP, 0); // 40%
    lv_obj_set_style_opa(c->obj, it->disabled ? 90 : LV_OPA_COVER, 0);  // 35%
    lv_obj_set_style_pad_row(c->obj, step ? 14 : 8, 0);
    set_tappable(c->obj, (sw || it->kind == DOCK_ITEM_SELECT) && !it->disabled);

    show(c->chip, !step);
    show(c->steps, step);
    show(c->text, !step);
    if (step) {
        render_step(c->dec, big, !it->disabled && !it->at_min);
        render_step(c->inc, big, !it->disabled && !it->at_max);
        lv_obj_set_style_text_font(c->value, font_get(FONT_MONO_BOLD, big ? 52 : 36), 0);
        lv_obj_set_width(c->value, big ? 140 : 100);
        set_text(c->value, it->text);
        pulse(c->chip, false);
    } else {
        int chip = big ? 96 : 56;
        lv_obj_set_size(c->chip, chip, chip);
        lv_obj_set_user_data(c->chip, (void *)(intptr_t)it->icon);
        bool tint = it->busy || lit;
        lv_obj_set_style_bg_color(c->chip, lv_color_hex(tint ? color : 0x1C1C1E), 0);
        lv_obj_set_style_bg_opa(c->chip, it->busy ? 36 : tint ? 41 : LV_OPA_COVER, 0); // 14%, 16%
        lv_obj_set_style_arc_color(c->chip, lv_color_hex(color), 0);
        lv_obj_set_style_line_color(c->chip, lv_color_hex(color), 0);
        lv_obj_invalidate(c->chip); // the icon may have changed with nothing else
        pulse(c->chip, it->busy);
        lv_obj_set_style_text_font(c->text, font_get(FONT_BOLD, big ? 20 : 16), 0);
        set_color(c->text, color);
        set_text(c->text, it->text);
    }

    const lv_font_t *fl = font_get(FONT_BOLD, big ? 26 : 20);
    lv_obj_set_style_text_font(c->label, fl, 0);
    lv_obj_set_size(c->label, w - 24, lv_font_get_line_height(fl));
    set_text(c->label, it->label);
}

static void render_fn(void)
{
    const dock_page_t *pg = fn_page();
    int n = pg ? pg->item_count : 0;
    int cols = n <= 3 ? (n > 1 ? n : 1) : n == 4 ? 2 : 3;
    int rows = n <= 3 ? 1 : 2;
    int w = (FN_W - (cols - 1) * CARD_GAP) / cols;
    int h = (FN_H - (rows - 1) * CARD_GAP) / rows;
    for (int i = 0; i < DOCK_MAX_ITEMS; i++) {
        card_t *c = &ui.cards[i];
        if (i < n) {
            render_card(c, &pg->items[i], i % cols * (w + CARD_GAP), i / cols * (h + CARD_GAP), w, h, rows == 1);
        } else {
            lv_obj_add_flag(c->obj, LV_OBJ_FLAG_HIDDEN);
            pulse(c->chip, false);
        }
    }
    if (n == 0) {
        lv_obj_remove_flag(ui.fn_empty, LV_OBJ_FLAG_HIDDEN);
    } else {
        lv_obj_add_flag(ui.fn_empty, LV_OBJ_FLAG_HIDDEN);
    }
}

// Shows the body of the page s_fn and the strip over it.
static void render_page(void)
{
    if (s_fn) {
        lv_obj_add_flag(ui.agent, LV_OBJ_FLAG_HIDDEN);
        lv_obj_remove_flag(ui.fn, LV_OBJ_FLAG_HIDDEN);
        render_fn();
    } else {
        lv_obj_remove_flag(ui.agent, LV_OBJ_FLAG_HIDDEN);
        lv_obj_add_flag(ui.fn, LV_OBJ_FLAG_HIDDEN);
        for (int i = 0; i < DOCK_MAX_ITEMS; i++) {
            pulse(ui.cards[i].chip, false);
        }
    }
    render_strip();
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
            lv_obj_align_to(m->window, m->reset, LV_ALIGN_OUT_RIGHT_MID, 0, 0);
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
    bool tall = is_tall(s);
    const char *one = tall ? "" : only_line(s);
    int h = tall ? MAIN_H_TALL : MAIN_H;
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

    // Project line: project name, session name in small type and provider
    // tag on the left; state, time and this turn's output
    // tokens ("1:55 ↓6.5K", like Claude Code's spinner) on the right. All
    // but the names stay whole; the session name gives way first, then the
    // project name.
    set_text(r->prov, PROVIDER_NAME[s->provider]);
    set_color(r->prov, PROVIDER_COLOR[s->provider]);
    set_text(r->label, s->label[0] ? s->label : STATE[s->state].label);
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

    int time_x = END_X - lv_obj_get_width(r->time) - (s->turn_output > 0 ? lv_obj_get_width(r->tok) : 0);
    int label_x = time_x - 8 - lv_obj_get_width(r->label);
    int room = label_x - 12 - MID_X - 8 - lv_obj_get_width(r->prov); // project and session name
    int pw = lv_obj_get_width(r->project);
    if (pw > room) {
        pw = room > 1 ? room : 1;
        lv_obj_set_width(r->project, pw);
    }
    int py = tall ? 5 : one[0] ? 4 : (h - lv_obj_get_height(r->project)) / 2;
    int base = py + ascent(r->project);
    lv_obj_set_pos(r->project, MID_X, py);
    int x = MID_X + pw + 8;
    int left = room - pw - 8;
    if (s->title[0] && left >= 32) {
        int nw = lv_obj_get_width(r->name);
        if (nw > left) {
            nw = left;
            lv_obj_set_width(r->name, nw);
        }
        lv_obj_remove_flag(r->name, LV_OBJ_FLAG_HIDDEN);
        on_baseline(r->name, x, base);
        x += nw + 8;
    } else {
        lv_obj_add_flag(r->name, LV_OBJ_FLAG_HIDDEN);
    }
    on_baseline(r->prov, x, base);
    on_baseline(r->label, label_x, base);
    on_baseline(r->time, time_x, base);
    on_baseline(r->tok, time_x + lv_obj_get_width(r->time), base);

    // Full-width lines below: in tall rows the assistant's latest text, then
    // what the session is doing; in short rows whichever there is.
    if (tall) {
        lv_obj_remove_flag(r->msg, LV_OBJ_FLAG_HIDDEN);
        set_text(r->msg, s->msg);
        lv_obj_set_pos(r->msg, MID_X, 35);
        lv_obj_set_style_text_font(r->detail, font_get(FONT_REGULAR, 15), 0);
        set_color(r->detail, C_TEXT3);
        set_text(r->detail, s->detail);
        lv_obj_set_pos(r->detail, MID_X, 56);
    } else {
        lv_obj_add_flag(r->msg, LV_OBJ_FLAG_HIDDEN);
        lv_obj_set_style_text_font(r->detail, font_get(FONT_REGULAR, 16), 0);
        set_color(r->detail, C_TEXT2);
        set_text(r->detail, one);
        lv_obj_set_pos(r->detail, MID_X, 34);
    }
    if (tall || one[0]) {
        lv_obj_remove_flag(r->detail, LV_OBJ_FLAG_HIDDEN);
    } else {
        lv_obj_add_flag(r->detail, LV_OBJ_FLAG_HIDDEN);
    }
    lv_obj_set_height(r->detail, lv_font_get_line_height(lv_obj_get_style_text_font(r->detail, 0)));
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
    int end = LIST_W - SUB_X - 12 - 56 - 6 - (labelled ? 64 + 6 : 0);
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

// Shows why there is nothing current to display, or hides the card.
static void render_overlay(int64_t now_us)
{
    int64_t last = link_last_rx_us();
    const char *title = NULL, *icon = LV_SYMBOL_USB;
    uint32_t icon_color = C_RED;
    char sub[112] = "", note[64] = "", ver[64];
    int pct = -1; // progress bar, hidden when < 0
    int progress;
    const char *err;
    ota_state_t ota = ota_status(&progress, &err);

    snprintf(ver, sizeof(ver), "固件 %s", ota_version());
    if (ota != OTA_IDLE) {
        // An update outranks everything: the user must not unplug now.
        icon = LV_SYMBOL_DOWNLOAD;
        icon_color = C_GREEN;
        snprintf(ver, sizeof(ver), "固件 %s → %s", ota_version(), ota_target());
        if (ota == OTA_PREPARING) {
            title = "正在升级固件";
            snprintf(sub, sizeof(sub), "正在准备… 请勿拔线");
            pct = 0;
        } else if (ota == OTA_RECEIVING) {
            title = "正在升级固件";
            snprintf(sub, sizeof(sub), "%d%% · 请勿拔线", progress);
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
    } else if (link_incompatible()) {
        title = "版本不兼容";
        snprintf(sub, sizeof(sub), "请把 go-dock 和固件升级到同一版本");
    } else if (last == 0) {
        title = "等待 go-dock";
        snprintf(sub, sizeof(sub), "用 USB 连接电脑并运行 go-dock");
    } else if (now_us - last > OFFLINE_AFTER_US) {
        title = "主机离线";
        int64_t sec = (now_us - last) / 1000000;
        if (sec < 120) {
            snprintf(sub, sizeof(sub), "已 %d 秒没有收到 go-dock 的数据", (int)sec);
        } else {
            snprintf(sub, sizeof(sub), "已 %d 分钟没有收到 go-dock 的数据", (int)(sec / 60));
        }
        if (s_have) {
            char hm[8];
            fmt_clock(hm, sizeof(hm), s_snap->now);
            snprintf(note, sizeof(note), "下面是 %s 的状态", hm);
        }
    }

    if (!title) {
        lv_obj_add_flag(ui.overlay, LV_OBJ_FLAG_HIDDEN);
        lv_obj_set_style_opa(ui.content, LV_OPA_COVER, 0);
        lv_obj_set_style_bg_color(ui.link_dot, lv_color_hex(C_GREEN), 0);
        return;
    }
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

void ui_poll(void)
{
    int64_t now_us = esp_timer_get_time();
    bool fresh = link_take_snapshot(s_snap, &s_version, &s_rx_us);
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
        for (int i = 0; i < s_snap->count; i++) {
            if (is_active(s_snap->sessions[i].state)) {
                s_active_us = now_us;
                break;
            }
        }
        render_strip();
        render_rows();
    }
    if (link_take_pages(s_pages, &s_pages_version)) {
        // Stay on the page shown while it is there, else go back to Agent.
        int keep = 0;
        for (int i = 0; s_fn_id[0] && i < s_pages->count; i++) {
            if (strcmp(s_pages->pages[i].id, s_fn_id) == 0) {
                keep = i + 1;
            }
        }
        show_fn(keep);
    }
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
        render_overlay(now_us);
        if (!fresh) {
            render_rows(); // elapsed times
        }
        shift_pixels(now_us);
    }
}

static uint8_t brightness(void)
{
    int64_t now_us = esp_timer_get_time();
    int64_t last = link_last_rx_us();
    if (ota_status(NULL, NULL) != OTA_IDLE) {
        return BRIGHT_FULL;
    }
    if (last == 0 || now_us - last > OFFLINE_AFTER_US) {
        bool long_gone = last == 0 ? now_us > 10 * 60 * 1000000LL : now_us - last > 10 * 60 * 1000000LL;
        return now_us - s_touch_us < TOUCH_WAKE_US ? BRIGHT_FULL : (long_gone ? BRIGHT_ASLEEP : BRIGHT_OFFLINE);
    }
    if (s_have && (s_snap->counts[DOCK_APPROVAL] || s_snap->counts[DOCK_INPUT] || s_snap->counts[DOCK_ERROR])) {
        return BRIGHT_FULL; // needs the user: never dim
    }
    if (now_us - s_touch_us < TOUCH_WAKE_US || now_us - s_active_us < DIM_AFTER_US) {
        return BRIGHT_FULL;
    }
    return BRIGHT_DIM;
}

uint8_t ui_brightness(void)
{
    s_bright = brightness();
    return s_bright;
}
