#include "ui.h"

#include <stdio.h>
#include <string.h>
#include <time.h>

#include "esp_heap_caps.h"
#include "esp_timer.h"
#include "fonts.h"
#include "link.h"
#include "lvgl.h"
#include "model.h"
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
// §7.1): the project line carries the session name; below it a session row
// is 80 px with the assistant's latest text and an action line, 60 px with
// one; each subagent adds a 32 px row under it; a session and its
// subagents never split across pages.
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
#define END_W 110 // state label and time column
#define MID_W (LIST_W - 14 - END_W - 8 - MID_X) // project line

typedef struct {
    lv_obj_t *reset, *window, *pct, *bar;
} meter_t;

typedef struct {
    lv_obj_t *obj, *chip, *icon, *spinner, *project, *name, *prov, *badge, *badge_text, *msg, *detail, *label, *time;
} row_t;

typedef struct {
    lv_obj_t *obj, *icon, *spinner, *type, *text, *label, *time;
} sub_t;

static struct {
    lv_obj_t *root;    // shifted a few px now and then against burn-in
    lv_obj_t *content; // dimmed when the host is away
    lv_obj_t *summary, *link_dot, *clock;
    struct {
        meter_t m[2];
        lv_obj_t *today;
    } usage[2];
    row_t rows[MAX_MAIN];
    sub_t subs[MAX_SUB];
    lv_obj_t *pager, *dots[MAX_DOTS];
    lv_obj_t *empty;
    lv_obj_t *overlay, *ov_title, *ov_sub, *ov_note;
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

static void fmt_elapsed(char *buf, size_t n, dock_state_t state, int64_t since, int64_t now)
{
    int64_t sec = now - since;
    if (sec < 0) {
        sec = 0;
    }
    if (is_active(state)) {
        if (sec >= 3600) {
            snprintf(buf, n, "%dh%02dm", (int)(sec / 3600), (int)(sec % 3600 / 60));
        } else {
            snprintf(buf, n, "%d:%02d", (int)(sec / 60), (int)(sec % 60));
        }
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

static bool is_tall(const dock_session_t *s)
{
    return s->msg[0] && s->detail[0];
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

static void build_strip(lv_obj_t *p)
{
    lv_obj_t *title = text(p, font_get(FONT_BOLD, 20), C_TEXT);
    lv_label_set_text(title, "Agent");
    lv_obj_set_pos(title, PAD_X, TOP);

    ui.summary = line(p, font_get(FONT_REGULAR, 15), C_TEXT2, 360);
    lv_obj_align_to(ui.summary, title, LV_ALIGN_OUT_RIGHT_BOTTOM, 12, -3);

    ui.clock = text(p, font_get(FONT_MONO, 20), C_TEXT);
    lv_label_set_text(ui.clock, "--:--");
    lv_obj_align(ui.clock, LV_ALIGN_TOP_RIGHT, -PAD_X, TOP + 2);

    ui.link_dot = box(p, 0, 0, 8, 8, C_GREEN, LV_RADIUS_CIRCLE);
    lv_obj_align_to(ui.link_dot, ui.clock, LV_ALIGN_OUT_LEFT_MID, -8, 0);
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

    // "子任务 N" when the session has subagents.
    r->badge = box(r->obj, 0, 0, 10, 10, C_TRACK, 7);
    lv_obj_set_size(r->badge, LV_SIZE_CONTENT, LV_SIZE_CONTENT);
    lv_obj_set_style_pad_hor(r->badge, 7, 0);
    lv_obj_set_style_pad_ver(r->badge, 1, 0);
    r->badge_text = text(r->badge, font_get(FONT_BOLD, 13), 0xC9C9C4);

    r->msg = line(r->obj, font_get(FONT_REGULAR, 15), 0xC9C9C4, MID_W);
    r->detail = line(r->obj, font_get(FONT_REGULAR, 16), C_TEXT2, MID_W);

    r->label = text(r->obj, font_get(FONT_BOLD, 18), C_TEXT);
    lv_obj_set_width(r->label, END_W);
    lv_obj_set_style_text_align(r->label, LV_TEXT_ALIGN_RIGHT, 0);
    r->time = text(r->obj, font_get(FONT_MONO, 15), C_TEXT3);
    lv_obj_set_width(r->time, END_W);
    lv_obj_set_style_text_align(r->time, LV_TEXT_ALIGN_RIGHT, 0);
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

static void on_pager(lv_event_t *e)
{
    (void)e;
    go_page(1);
    render_rows();
}

static void on_gesture(lv_event_t *e)
{
    (void)e;
    lv_dir_t dir = lv_indev_get_gesture_dir(lv_indev_active());
    // Up/down pages through tasks. Left/right will switch function pages
    // once there is more than the Agent page.
    if (dir == LV_DIR_TOP) {
        go_page(1);
        render_rows();
    } else if (dir == LV_DIR_BOTTOM) {
        go_page(-1);
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

static void build_overlay(lv_obj_t *p)
{
    ui.overlay = box(p, 0, 0, 380, 180, 0x161617, 22);
    lv_obj_center(ui.overlay);
    lv_obj_set_style_border_width(ui.overlay, 1, 0);
    lv_obj_set_style_border_color(ui.overlay, lv_color_hex(0x2C2C2E), 0);
    lv_obj_t *icon = text(ui.overlay, &lv_font_montserrat_28, C_RED);
    lv_label_set_text(icon, LV_SYMBOL_USB);
    lv_obj_align(icon, LV_ALIGN_TOP_MID, 0, 18);
    ui.ov_title = text(ui.overlay, font_get(FONT_BOLD, 28), C_TEXT);
    lv_obj_align(ui.ov_title, LV_ALIGN_TOP_MID, 0, 54);
    ui.ov_sub = text(ui.overlay, font_get(FONT_REGULAR, 17), C_TEXT2);
    lv_obj_align(ui.ov_sub, LV_ALIGN_TOP_MID, 0, 100);
    ui.ov_note = text(ui.overlay, font_get(FONT_REGULAR, 15), C_TEXT4);
    lv_obj_align(ui.ov_note, LV_ALIGN_TOP_MID, 0, 130);
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
    build_usage(ui.content);
    build_tasks(ui.content);
    build_overlay(ui.root);

    int64_t now = esp_timer_get_time();
    s_touch_us = s_active_us = s_shift_us = now;
    render_rows();
}

// ---------- rendering ----------

static void render_summary(void)
{
    char buf[96] = "";
    if (s_have) {
        static const dock_state_t order[] = {DOCK_APPROVAL, DOCK_INPUT, DOCK_ERROR, DOCK_RUNNING};
        size_t used = 0;
        for (size_t i = 0; i < sizeof(order) / sizeof(order[0]); i++) {
            int n = s_snap->counts[order[i]];
            if (n > 0 && used < sizeof(buf)) {
                used += snprintf(buf + used, sizeof(buf) - used, "%s%s %d", used ? " · " : "", STATE[order[i]].label, n);
            }
        }
        if (!used && s_snap->count > 0) {
            snprintf(buf, sizeof(buf), "全部完成");
        }
    }
    set_text(ui.summary, buf);
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

    // Project line: project name, session name in small type, provider tag,
    // subagent badge. The tags stay whole; the session name gives way
    // first, then the project name.
    set_text(r->prov, PROVIDER_NAME[s->provider]);
    set_color(r->prov, PROVIDER_COLOR[s->provider]);
    if (s->agents_total > 0) {
        snprintf(buf, sizeof(buf), "子任务 %d", s->agents_active > 0 ? s->agents_active : s->agents_total);
        set_text(r->badge_text, buf);
        lv_obj_remove_flag(r->badge, LV_OBJ_FLAG_HIDDEN);
    } else {
        lv_obj_add_flag(r->badge, LV_OBJ_FLAG_HIDDEN);
    }
    lv_obj_set_width(r->project, LV_SIZE_CONTENT);
    set_text(r->project, s->project[0] ? s->project : "-");
    lv_obj_set_pos(r->project, MID_X, tall ? 5 : 4);
    lv_obj_set_width(r->name, LV_SIZE_CONTENT);
    set_text(r->name, s->title);
    lv_obj_update_layout(r->obj);
    int room = MID_W - 8 - lv_obj_get_width(r->prov) - (s->agents_total > 0 ? 8 + lv_obj_get_width(r->badge) : 0);
    if (lv_obj_get_width(r->project) > room) {
        lv_obj_set_width(r->project, room);
    }
    int left = room - lv_obj_get_width(r->project) - 8;
    if (s->title[0] && left >= 32) {
        if (lv_obj_get_width(r->name) > left) {
            lv_obj_set_width(r->name, left);
        }
        lv_obj_remove_flag(r->name, LV_OBJ_FLAG_HIDDEN);
        lv_obj_align_to(r->name, r->project, LV_ALIGN_OUT_RIGHT_BOTTOM, 8, -4);
        lv_obj_align_to(r->prov, r->name, LV_ALIGN_OUT_RIGHT_BOTTOM, 8, 0);
    } else {
        lv_obj_add_flag(r->name, LV_OBJ_FLAG_HIDDEN);
        lv_obj_align_to(r->prov, r->project, LV_ALIGN_OUT_RIGHT_BOTTOM, 8, -4);
    }
    lv_obj_align_to(r->badge, r->prov, LV_ALIGN_OUT_RIGHT_MID, 8, 0);

    // Tall rows: the assistant's latest text, then what the session is
    // doing. Short rows: one line, whichever there is (older hosts send
    // only text, which may repeat the session name).
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
        const char *one = s->msg[0] ? s->msg : s->detail[0] ? s->detail : strcmp(s->text, s->title) ? s->text : "";
        set_text(r->detail, one[0] ? one : " ");
        lv_obj_set_pos(r->detail, MID_X, 34);
    }
    lv_obj_set_height(r->detail, lv_font_get_line_height(lv_obj_get_style_text_font(r->detail, 0)));

    set_text(r->label, s->label[0] ? s->label : STATE[s->state].label);
    set_color(r->label, color);
    lv_obj_set_pos(r->label, LIST_W - 14 - END_W, tall ? 8 : 6);
    fmt_elapsed(buf, sizeof(buf), s->state, s->since, now);
    set_text(r->time, buf);
    lv_obj_set_pos(r->time, LIST_W - 14 - END_W, tall ? 55 : 35);
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
    const char *title = NULL;
    char sub[64] = "", note[64] = "";

    if (link_incompatible()) {
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
    set_text(ui.ov_title, title);
    set_text(ui.ov_sub, sub);
    set_text(ui.ov_note, note);
    lv_obj_align(ui.ov_title, LV_ALIGN_TOP_MID, 0, 54);
    lv_obj_align(ui.ov_sub, LV_ALIGN_TOP_MID, 0, 100);
    lv_obj_align(ui.ov_note, LV_ALIGN_TOP_MID, 0, 130);
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
        render_summary();
        render_rows();
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

uint8_t ui_brightness(void)
{
    int64_t now_us = esp_timer_get_time();
    int64_t last = link_last_rx_us();
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
