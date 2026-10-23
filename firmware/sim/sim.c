// Desktop renderer for the firmware UI: feeds main/ui.c sample snapshots
// and writes what the 600x450 panel would show as PPM images.
//
//   dock_sim <out-dir>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "fonts.h"
#include "link.h"
#include "lvgl.h"
#include "ota.h"
#include "settings.h"
#include "ui.h"

#define W 600
#define H 450
#define SEC 1000000LL
#define FULL (80 * 255 / 100) // the panel level at the default brightness

// ---- stand-ins for the ESP-IDF pieces ui.c uses ----

static int64_t s_now_us = 600 * SEC; // pretend the board has been up 10 min
static dock_snapshot_t s_snap;
static uint32_t s_ver;
static int64_t s_rx_us, s_last_rx_us;

int64_t esp_timer_get_time(void) { return s_now_us; }

bool link_take_snapshot(dock_snapshot_t *out, uint32_t *version, int64_t *received_us)
{
    if (*version == s_ver) {
        return false;
    }
    memcpy(out, &s_snap, sizeof(*out));
    *version = s_ver;
    *received_us = s_rx_us;
    return true;
}

int64_t link_last_rx_us(void) { return s_last_rx_us; }
bool link_incompatible(void) { return false; }

static dock_pages_t s_pages;
static uint32_t s_pages_ver;

bool link_take_pages(dock_pages_t *out, uint32_t *version)
{
    if (*version == s_pages_ver) {
        return false;
    }
    memcpy(out, &s_pages, sizeof(*out));
    *version = s_pages_ver;
    return true;
}

static int s_actions;
static char s_action[96];

void link_send_action(const char *page, const char *item, const char *act)
{
    s_actions++;
    snprintf(s_action, sizeof(s_action), "%s/%s %s", page, item, act);
    printf("action %s\n", s_action);
}

void link_send_swap(const char *page, const char *item, const char *to)
{
    s_actions++;
    snprintf(s_action, sizeof(s_action), "%s/%s swap %s", page, item, to);
    printf("action %s\n", s_action);
}

static settings_t s_settings = {.brightness = 80, .dim_brightness = 20, .dim_after = 300};

settings_t settings_get(void) { return s_settings; }

static ota_state_t s_ota;
static int s_ota_pct;

ota_state_t ota_status(int *percent, const char **error)
{
    if (percent) {
        *percent = s_ota_pct;
    }
    if (error) {
        *error = "写入失败（ESP_ERR_FLASH_OP_FAIL）";
    }
    return s_ota;
}

const char *ota_version(void) { return "0.1.4"; }
const char *ota_target(void) { return "0.1.5"; }

// Fonts load from the TTF files instead of embedded binaries.
static void *load(const char *name, size_t *size)
{
    char path[512];
    snprintf(path, sizeof(path), "%s/%s", FONT_DIR, name);
    FILE *f = fopen(path, "rb");
    if (!f) {
        fprintf(stderr, "cannot open %s\n", path);
        exit(1);
    }
    fseek(f, 0, SEEK_END);
    *size = (size_t)ftell(f);
    fseek(f, 0, SEEK_SET);
    void *buf = malloc(*size);
    fread(buf, 1, *size, f);
    fclose(f);
    return buf;
}

const lv_font_t *font_get(font_face_t face, int size)
{
    static const char *files[] = {"NotoSansSC-Regular.ttf", "NotoSansSC-Bold.ttf", "JetBrainsMono-Medium.ttf",
                                  "JetBrainsMono-Bold.ttf"};
    static void *data[4];
    static size_t len[4];
    static lv_font_t *cache[4][64];
    if (!cache[face][size]) {
        if (!data[face]) {
            data[face] = load(files[face], &len[face]);
        }
        cache[face][size] = lv_tiny_ttf_create_data_ex(data[face], len[face], size, LV_FONT_KERNING_NONE, 96);
        if (face != FONT_REGULAR) {
            cache[face][size]->fallback = font_get(FONT_REGULAR, size);
        }
    }
    return cache[face][size];
}

// ---- framebuffer ----

static uint16_t s_fb[W * H];

static void flush(lv_display_t *d, const lv_area_t *a, uint8_t *px)
{
    const uint16_t *src = (const uint16_t *)px;
    for (int y = a->y1; y <= a->y2; y++) {
        memcpy(&s_fb[y * W + a->x1], src, (size_t)(a->x2 - a->x1 + 1) * 2);
        src += a->x2 - a->x1 + 1;
    }
    lv_display_flush_ready(d);
}

static void save(const char *dir, const char *name)
{
    char path[512];
    snprintf(path, sizeof(path), "%s/%s.ppm", dir, name);
    FILE *f = fopen(path, "wb");
    fprintf(f, "P6\n%d %d\n255\n", W, H);
    for (int i = 0; i < W * H; i++) {
        uint16_t c = s_fb[i];
        uint8_t rgb[3] = {(uint8_t)((c >> 11) << 3), (uint8_t)(((c >> 5) & 0x3F) << 2), (uint8_t)((c & 0x1F) << 3)};
        fwrite(rgb, 1, 3, f);
    }
    fclose(f);
    printf("%s\n", path);
}

static uint32_t tick(void) { return (uint32_t)(s_now_us / 1000); }

// One pass of the firmware's LVGL task, 50 ms on.
static void step(void)
{
    s_now_us += 50000;
    ui_poll();
    ui_brightness();
    lv_timer_handler();
}

static void run(int64_t us)
{
    for (int64_t t = 0; t < us; t += 50000) {
        step();
    }
    lv_refr_now(NULL);
}

// ---- touch, through LVGL's own gesture and click handling ----

static bool s_pressed;
static lv_point_t s_point;

static void touch_read(lv_indev_t *indev, lv_indev_data_t *data)
{
    (void)indev;
    data->point = s_point;
    data->state = s_pressed ? LV_INDEV_STATE_PRESSED : LV_INDEV_STATE_RELEASED;
    if (s_pressed) {
        ui_touched(); // as the board's touch driver does
    }
}

static void tap(int x, int y)
{
    s_point = (lv_point_t){x, y};
    s_pressed = true;
    step();
    step();
    s_pressed = false;
    step();
    step();
}

static void swipe(int x, int y, int dx, int dy)
{
    s_point = (lv_point_t){x, y};
    s_pressed = true;
    step();
    for (int i = 1; i <= 6; i++) {
        s_point = (lv_point_t){x + dx * i / 6, y + dy * i / 6};
        step();
    }
    s_pressed = false;
    step();
    step();
}

// Holds a finger at x, y for hold_us, then drags it to to_x, to_y and
// keeps it there; release() lets go.
static void drag(int x, int y, int to_x, int to_y, int64_t hold_us)
{
    s_point = (lv_point_t){x, y};
    s_pressed = true;
    for (int64_t t = 0; t <= hold_us; t += 50000) {
        step();
    }
    for (int i = 1; i <= 8; i++) {
        s_point = (lv_point_t){x + (to_x - x) * i / 8, y + (to_y - y) * i / 8};
        step();
    }
}

static void release(void)
{
    s_pressed = false;
    step();
    step();
}

static void check(bool ok, const char *what)
{
    if (!ok) {
        fprintf(stderr, "FAIL: %s\n", what);
        exit(1);
    }
}

// ---- sample data, as go-dock would send it ----

static const int64_t NOW = 1792202470; // host clock
static const int TZ = 8 * 3600;

static void add(dock_state_t st, dock_provider_t p, const char *project, const char *label, const char *text, int ago)
{
    dock_session_t *s = &s_snap.sessions[s_snap.count++];
    snprintf(s->id, sizeof(s->id), "%s:%s", p == DOCK_CLAUDE ? "claude" : "codex", project);
    s->provider = p;
    s->state = st;
    snprintf(s->project, sizeof(s->project), "%s", project);
    snprintf(s->label, sizeof(s->label), "%s", label);
    snprintf(s->text, sizeof(s->text), "%s", text);
    s->since = NOW - ago;
    s_snap.counts[st]++;
}

static dock_session_t *last(void) { return &s_snap.sessions[s_snap.count - 1]; }
static void title(const char *t) { snprintf(last()->title, sizeof(last()->title), "%s", t); }
static void detail(const char *t) { snprintf(last()->detail, sizeof(last()->detail), "%s", t); }
static void msg(const char *t) { snprintf(last()->msg, sizeof(last()->msg), "%s", t); }

static void turn(int secs, int64_t output)
{
    last()->turn_secs = secs;
    last()->turn_output = output;
}

static void agent(dock_state_t st, const char *type, const char *text, int ago)
{
    dock_session_t *s = last();
    dock_agent_t *a = &s->agents[s->agent_count++];
    snprintf(a->type, sizeof(a->type), "%s", type);
    snprintf(a->text, sizeof(a->text), "%s", text);
    a->state = st;
    a->since = NOW - ago;
    s->agents_total++;
    if (st != DOCK_DONE) {
        s->agents_active++;
    }
}

static void window(dock_window_t *w, float pct, int resets_in)
{
    w->present = true;
    w->pct = pct;
    w->resets_at = NOW + resets_in;
}

static void send(void)
{
    s_snap.seq++;
    s_snap.now = NOW;
    s_snap.tz_offset = TZ;
    s_ver++;
    s_rx_us = s_last_rx_us = s_now_us;
}

static void reset(void) { memset(&s_snap, 0, sizeof(s_snap)); }

// Function pages, as the lights plugin describes them.
static void page(const char *id, const char *title, const char *summary, const char *error)
{
    dock_page_t *p = &s_pages.pages[s_pages.count++];
    memset(p, 0, sizeof(*p));
    snprintf(p->id, sizeof(p->id), "%s", id);
    snprintf(p->title, sizeof(p->title), "%s", title);
    snprintf(p->summary, sizeof(p->summary), "%s", summary);
    snprintf(p->error, sizeof(p->error), "%s", error);
}

// A lamp as the lights plugin sends it, with 开 and 关; on: 1, 0 or -1
// (unknown); text NULL says what on and busy mean.
static void lamp(const char *id, const char *label, int on, bool busy, bool disabled, const char *text)
{
    dock_page_t *p = &s_pages.pages[s_pages.count - 1];
    dock_item_t *it = &p->items[p->item_count++];
    snprintf(it->id, sizeof(it->id), "%s", id);
    it->kind = DOCK_ITEM_SWITCH;
    it->buttons = true;
    snprintf(it->label, sizeof(it->label), "%s", label);
    it->on = (int8_t)on;
    it->busy = busy;
    it->disabled = disabled;
    if (!text) {
        text = busy ? "切换中…" : on == 1 ? "已开" : on == 0 ? "已关" : "未知";
    }
    snprintf(it->text, sizeof(it->text), "%s", text);
}

// Any widget, unknown state.
static dock_item_t *item(dock_item_kind_t kind, dock_icon_t icon, const char *id, const char *label, const char *text)
{
    dock_page_t *p = &s_pages.pages[s_pages.count - 1];
    dock_item_t *it = &p->items[p->item_count++];
    memset(it, 0, sizeof(*it));
    it->kind = kind;
    it->icon = icon;
    it->on = -1;
    snprintf(it->id, sizeof(it->id), "%s", id);
    snprintf(it->label, sizeof(it->label), "%s", label);
    snprintf(it->text, sizeof(it->text), "%s", text);
    return it;
}

static void reset_pages(void);

// The air conditioner page as the aircon plugin describes it.
static void aircon(const char *summary, const char *temp, dock_icon_t mode_icon, const char *mode, const char *fan)
{
    reset_pages();
    page("aircon", "空调", summary, "");
    item(DOCK_ITEM_SWITCH, DOCK_ICON_POWER, "power", "电源", "已开")->on = 1;
    item(DOCK_ITEM_STEPPER, DOCK_ICON_BULB, "temp", "温度", temp);
    item(DOCK_ITEM_SELECT, mode_icon, "mode", "模式", mode);
    item(DOCK_ITEM_SELECT, DOCK_ICON_FAN, "fan", "风速", fan);
}

// A meter as the 性能 page sends it.
static dock_item_t *meter(dock_icon_t icon, const char *id, const char *label, const char *text, const char *unit, const char *sub)
{
    dock_item_t *it = item(DOCK_ITEM_METER, icon, id, label, text);
    snprintf(it->unit, sizeof(it->unit), "%s", unit);
    snprintf(it->sub, sizeof(it->sub), "%s", sub);
    it->pct = -1;
    return it;
}

// A line of n readings from v, rising by step and wrapping into 10-90.
static void line_of(dock_hist_t *h, int n, int v, int step)
{
    h->n = (uint8_t)n;
    for (int i = 0; i < n; i++) {
        h->v[i] = (uint8_t)(10 + (v + i * step) % 80);
    }
}

static void series(dock_item_t *it, dock_color_t c, int v, int step)
{
    dock_series_t *s = &it->series[it->series_count++];
    s->color = c;
    line_of(&s->hist, 30, v, step);
}

static void row(dock_item_t *it, const char *label, const char *text, dock_color_t c)
{
    dock_row_t *r = &it->rows[it->row_count++];
    snprintf(r->label, sizeof(r->label), "%s", label);
    snprintf(r->text, sizeof(r->text), "%s", text);
    r->color = c;
}

// The 性能 page as the perf plugin describes it: CPU and GPU big, memory,
// fans, network and disk small; mem_big swaps memory and the GPU.
static void perf(bool mem_big)
{
    reset_pages();
    page("perf", "性能", "已运行 19 小时", "");
    s_pages.pages[0].movable = true;
    dock_item_t *cpu = meter(DOCK_ICON_CPU, "cpu", "CPU", "37", "%", "49°C");
    cpu->big = true;
    snprintf(cpu->note, sizeof(cpu->note), "Apple M3 Max");
    series(cpu, DOCK_COLOR_BLUE, 10, 3);
    series(cpu, DOCK_COLOR_RED, 25, 3);
    row(cpu, "用户", "27%", DOCK_COLOR_BLUE);
    row(cpu, "系统", "10%", DOCK_COLOR_RED);
    row(cpu, "闲置", "63%", DOCK_COLOR_GRAY);
    dock_item_t *mem;
    if (mem_big) {
        mem = meter(DOCK_ICON_MEMORY, "mem", "内存", "92", "%", "压力偏高");
        mem->big = true;
        mem->level = DOCK_LEVEL_WARN;
        snprintf(mem->note, sizeof(mem->note), "共 36G");
        series(mem, DOCK_COLOR_BLUE, 40, 0);
        series(mem, DOCK_COLOR_ORANGE, 52, 0);
        series(mem, DOCK_COLOR_PURPLE, 75, 1);
        row(mem, "App", "15G", DOCK_COLOR_BLUE);
        row(mem, "联动", "3.8G", DOCK_COLOR_ORANGE);
        row(mem, "已压缩", "13G", DOCK_COLOR_PURPLE);
        row(mem, "交换", "2.1G", DOCK_COLOR_GRAY);
    }
    dock_item_t *gpu = meter(DOCK_ICON_GPU, "gpu", "GPU", "5", "%", "30 核");
    if (!mem_big) {
        gpu->big = true;
        snprintf(gpu->note, sizeof(gpu->note), "Apple M3 Max");
        series(gpu, DOCK_COLOR_TEAL, 5, 1);
        series(gpu, DOCK_COLOR_RED, 12, 1);
        series(gpu, DOCK_COLOR_BLUE, 20, 2);
        row(gpu, "利用率", "5%", DOCK_COLOR_BLUE);
        row(gpu, "渲染", "4%", DOCK_COLOR_RED);
        row(gpu, "Tiler", "5%", DOCK_COLOR_TEAL);
        mem = meter(DOCK_ICON_MEMORY, "mem", "内存", "62", "%", "压缩 4.9G");
        mem->pct = 62;
    } else {
        line_of(&gpu->hist, 30, 5, 2);
    }
    meter(DOCK_ICON_ROTOR, "fan", "风扇", "停转", "", "2 个")->pct = 0;
    line_of(&meter(DOCK_ICON_NET, "net", "网络", "22", "KB/s", "↑ 6.1K")->hist, 24, 0, 7);
    meter(DOCK_ICON_DISK, "disk", "磁盘", "90", "%", "剩 97G")->pct = 90;
}

static void tapped(int x, int y, const char *want, const char *what)
{
    int n = s_actions;
    tap(x, y);
    check(want ? s_actions == n + 1 && strcmp(s_action, want) == 0 : s_actions == n, what);
}

static void send_pages(void)
{
    s_pages_ver++;
    s_last_rx_us = s_now_us;
}

static void reset_pages(void) { memset(&s_pages, 0, sizeof(s_pages)); }

int main(int argc, char **argv)
{
    const char *out = argc > 1 ? argv[1] : ".";
    lv_init();
    lv_tick_set_cb(tick);
    lv_display_t *disp = lv_display_create(W, H);
    static uint16_t buf[W * 60];
    lv_display_set_buffers(disp, buf, NULL, sizeof(buf), LV_DISPLAY_RENDER_MODE_PARTIAL);
    lv_display_set_flush_cb(disp, flush);
    lv_indev_t *touch = lv_indev_create();
    lv_indev_set_type(touch, LV_INDEV_TYPE_POINTER);
    lv_indev_set_read_cb(touch, touch_read);
    ui_create();

    // Before go-dock talks to the board.
    run(2 * SEC);
    save(out, "0-waiting");

    // Everyday mix, two pages of tasks.
    reset();
    window(&s_snap.usage[DOCK_CLAUDE].five_hour, 62, 4 * 3600 + 33 * 60);
    window(&s_snap.usage[DOCK_CLAUDE].seven_day, 31, 4 * 86400 + 14 * 3600);
    s_snap.usage[DOCK_CLAUDE].today_tokens = 1840000;
    window(&s_snap.usage[DOCK_CODEX].five_hour, 28, 3 * 3600 + 5 * 60);
    window(&s_snap.usage[DOCK_CODEX].seven_day, 12, 5 * 86400);
    s_snap.usage[DOCK_CODEX].today_tokens = 402000;
    add(DOCK_APPROVAL, DOCK_CLAUDE, "go-dock", "需授权", "Explore · Bash: cat ~/.ssh/config", 42);
    agent(DOCK_APPROVAL, "Explore", "Bash: cat ~/.ssh/config", 18);
    agent(DOCK_RUNNING, "Explore", "Explore host tree toolbar & store", 186);
    agent(DOCK_RUNNING, "Explore", "Explore tables, dialogs, checkboxes", 178);
    detail("AskUserQuestion");
    add(DOCK_INPUT, DOCK_CLAUDE, "infra", "等待输入", "请选择部署区域", 65);
    add(DOCK_RUNNING, DOCK_CODEX, "api-svc", "进行中", "pytest -q tests/auth", 192);
    title("重构登录模块，拆分 token 校验");
    msg("先跑一遍认证相关的测试，看哪些用例失败。");
    detail("Bash: pytest -q tests/auth");
    add(DOCK_RUNNING, DOCK_CLAUDE, "另一个项目", "进行中", "Edit: launch.md", 45);
    add(DOCK_DONE, DOCK_CODEX, "scripts", "已完成", "迁移数据库脚本", 120);
    msg("迁移脚本已完成，共更新 3 张表。");
    add(DOCK_DONE, DOCK_CLAUDE, "dotfiles", "已完成", "整理 zsh 配置", 540);
    send();
    run(2 * SEC);
    save(out, "1-mixed");

    // Near the limit, with an error; Codex shows the widest usage texts.
    reset();
    window(&s_snap.usage[DOCK_CLAUDE].five_hour, 94, 38 * 60);
    window(&s_snap.usage[DOCK_CLAUDE].seven_day, 71, 2 * 86400);
    s_snap.usage[DOCK_CLAUDE].today_tokens = 3620000;
    window(&s_snap.usage[DOCK_CODEX].five_hour, 100, 4 * 3600 + 33 * 60);
    window(&s_snap.usage[DOCK_CODEX].seven_day, 100, 4 * 86400 + 14 * 3600);
    s_snap.usage[DOCK_CODEX].today_tokens = 12300000;
    add(DOCK_ERROR, DOCK_CLAUDE, "go-dock", "出错", "额度已用完，0:38 后恢复", 62);
    add(DOCK_APPROVAL, DOCK_CODEX, "api-svc", "需授权", "Bash: rm -rf dist", 15);
    add(DOCK_RUNNING, DOCK_CODEX, "a-very-long-project-name-that-overflows", "进行中",
        "Edit: src/components/navigation/HeaderWithAVeryLongName.tsx", 288);
    agent(DOCK_RUNNING, "general-purpose", "迁移导航组件到新版路由", 120);
    agent(DOCK_DONE, "Explore", "查找旧路由用法", 300);
    send();
    run(2 * SEC);
    save(out, "2-limit");

    // Nothing running.
    reset();
    window(&s_snap.usage[DOCK_CLAUDE].five_hour, 8, 4 * 3600 + 41 * 60);
    window(&s_snap.usage[DOCK_CLAUDE].seven_day, 22, 4 * 86400);
    s_snap.usage[DOCK_CLAUDE].today_tokens = 96000;
    send();
    run(2 * SEC);
    save(out, "3-empty");

    // Main turn ended while background subagents still work.
    reset();
    window(&s_snap.usage[DOCK_CLAUDE].five_hour, 41, 3 * 3600 + 20 * 60);
    window(&s_snap.usage[DOCK_CLAUDE].seven_day, 18, 4 * 86400 + 15 * 3600);
    add(DOCK_RUNNING, DOCK_CLAUDE, "host-app", "进行中", "Explore · Explore toolbar", 18);
    title("规划 host 端工具栏");
    msg("派两个 Explore 子任务分别查看工具栏和表格组件。");
    detail("等待 2 个子任务完成");
    agent(DOCK_RUNNING, "Explore", "Explore toolbar & store", 18);
    agent(DOCK_RUNNING, "Explore", "Explore tables, dialogs", 18);
    add(DOCK_DONE, DOCK_CODEX, "scripts", "已完成", "迁移数据库脚本", 300);
    send();
    run(2 * SEC);
    save(out, "5-background");

    // Under the project line what the session is doing, else the
    // assistant's latest text.
    reset();
    window(&s_snap.usage[DOCK_CLAUDE].five_hour, 41, 4 * 3600 + 33 * 60);
    window(&s_snap.usage[DOCK_CLAUDE].seven_day, 18, 4 * 86400 + 14 * 3600);
    add(DOCK_RUNNING, DOCK_CLAUDE, "shellrs", "进行中", "Finding the red colon's color in both screenshots", 114);
    title("detail");
    msg("先把两张截图的像素颜色取出来，对比 ShellRS 的配色和渲染。");
    detail("Finding the red colon's color in both screenshots");
    add(DOCK_RUNNING, DOCK_CLAUDE, "a-very-long-project-name", "进行中", "Read: ui.c", 30);
    title("一个很长很长的会话名字会被截断");
    agent(DOCK_RUNNING, "Explore", "查找字体渲染代码", 20);
    add(DOCK_DONE, DOCK_CODEX, "scripts", "已完成", "迁移数据库脚本", 300);
    title("迁移数据库脚本");
    msg("迁移脚本已完成，共更新 3 张表。");
    send();
    run(2 * SEC);
    save(out, "6-message");

    // Two sessions fill a page; swiping up shows the next two. Symbols
    // beyond GB2312's hanzi (§, •, –, ≥, ✓) have glyphs.
    reset();
    window(&s_snap.usage[DOCK_CLAUDE].five_hour, 41, 4 * 3600 + 33 * 60);
    window(&s_snap.usage[DOCK_CLAUDE].seven_day, 18, 4 * 86400 + 14 * 3600);
    add(DOCK_RUNNING, DOCK_CLAUDE, "go-dock", "进行中", "Bash: idf.py build", 95);
    title("硬件刷机");
    turn(95, 6500);
    detail("Bash: idf.py build");
    add(DOCK_DONE, DOCK_CLAUDE, "host-app", "已完成", "", 300);
    title("整理布局文档");
    msg("§7.1 的布局说明已更新 • 行高 126px");
    add(DOCK_DONE, DOCK_CODEX, "scripts", "已完成", "", 900);
    msg("迁移完成 ✓ 3 张表 – 耗时 ≥ 2 分钟");
    add(DOCK_DONE, DOCK_CLAUDE, "dotfiles", "已完成", "", 3600);
    msg("zsh 配置整理好了，去掉了重复的 PATH。");
    send();
    run(2 * SEC);
    save(out, "22-two-a-page");
    swipe(300, 300, 0, -150);
    run(1 * SEC);
    save(out, "23-two-a-page-next");

    // Turn clock and output tokens on the second line: running counts on
    // from turn_secs, approval holds it; a row with no text is centred.
    reset();
    window(&s_snap.usage[DOCK_CLAUDE].five_hour, 41, 4 * 3600 + 33 * 60);
    window(&s_snap.usage[DOCK_CLAUDE].seven_day, 18, 4 * 86400 + 14 * 3600);
    add(DOCK_APPROVAL, DOCK_CLAUDE, "api-svc", "需授权", "Bash: rm -rf dist", 15);
    turn(42, 1200);
    detail("Bash: rm -rf dist");
    add(DOCK_RUNNING, DOCK_CLAUDE, "shellrs", "进行中", "Finding the red colon's color", 125);
    title("detail");
    turn(125, 6500);
    msg("先把两张截图的像素颜色取出来，对比 ShellRS 和 iTerm2 的配色、字体渲染和行高。");
    detail("Finding the red colon's color in both screenshots and comparing them");
    add(DOCK_DONE, DOCK_CODEX, "notes", "已完成", "", 900);
    send();
    run(2 * SEC);
    save(out, "7-turn");

    // Function pages: the lights next to the Agent page, which now shows
    // page dots in its strip.
    reset_pages();
    page("lights", "灯光", "1 / 3 盏亮着 · 1 盏未知", "");
    lamp("main", "办公室主灯", 1, false, false, NULL);
    lamp("desk", "台灯", 0, false, false, NULL);
    lamp("fan", "风扇灯", -1, false, false, NULL);
    send_pages();
    run(1 * SEC);
    save(out, "10-agent-dots");

    // Swiping left shows the lights; 开 and 关 are sent as they are and show
    // the lamp switching until the host's next pages; the rest of the card
    // takes no taps.
    swipe(500, 250, -300, 0);
    run(1 * SEC);
    save(out, "11-lights-3");
    tapped(18 + 192 + 90, 160, NULL, "a tap beside the buttons does nothing");
    tapped(18 + 192 + 48, 337, "lights/desk on", "开 sends on");
    run(1 * SEC);
    save(out, "12-lights-tap");

    // A swipe that starts on a card only switches pages.
    swipe(110, 244, 300, 0);
    check(s_actions == 1, "a swipe is not a tap");
    swipe(500, 250, -300, 0);
    check(s_actions == 1, "a swipe is not a tap");

    reset_pages();
    page("lights", "灯光", "1 / 1 盏亮着", "");
    lamp("main", "办公室主灯", 1, false, false, NULL);
    send_pages();
    run(1 * SEC);
    save(out, "13-lights-1");

    // The broker is down: the reason in red, the cards greyed and deaf.
    reset_pages();
    page("lights", "灯光", "2 / 4 盏亮着", "MQTT 未连接");
    lamp("main", "办公室主灯", 1, false, true, NULL);
    lamp("desk", "台灯", 0, false, true, NULL);
    lamp("hall", "过道灯", 1, false, true, NULL);
    lamp("fan", "会议室吊灯很长的名字", 0, false, true, "未响应");
    send_pages();
    run(1 * SEC);
    save(out, "14-lights-4-offline");
    tap(18 + 138, 54 + 92);
    check(s_actions == 1, "a disabled card takes no taps");
    tapped(18 + 36 + 48, 203, NULL, "a disabled card's buttons take no taps");

    reset_pages();
    page("lights", "灯光", "2 / 6 盏亮着 · 1 盏未知", "");
    lamp("main", "办公室主灯", 1, false, false, NULL);
    lamp("desk", "台灯", 0, true, false, NULL);
    lamp("hall", "过道灯", 0, false, false, NULL);
    lamp("meet", "会议室", 1, false, false, NULL);
    lamp("fan", "风扇灯", -1, false, false, NULL);
    lamp("door", "门口", 0, false, false, "发送失败");
    send_pages();
    run(1 * SEC);
    save(out, "15-lights-6");

    // A screen dimmed for want of activity: the first tap only wakes it.
    reset();
    add(DOCK_DONE, DOCK_CODEX, "notes", "已完成", "", 900);
    s_now_us += 6 * 60 * SEC;
    send();
    send_pages();
    run(1 * SEC);
    check(ui_brightness() == 20 * 255 / 100, "dim after five idle minutes");
    tap(18 + 48, 203);
    check(s_actions == 1, "the waking tap switches nothing");
    tap(18 + 132, 203);
    check(s_actions == 2 && strcmp(s_action, "lights/main off") == 0, "the next tap switches off");

    // From a dim Agent page a swipe wakes the screen and shows the lights;
    // the screen stays lit and the buttons answer.
    swipe(100, 250, 300, 0);
    run(1 * SEC);
    s_now_us += 6 * 60 * SEC;
    send();
    send_pages();
    run(1 * SEC);
    check(ui_brightness() < FULL, "dim again on the Agent page");
    swipe(500, 250, -300, 0);
    run(2 * SEC);
    check(ui_brightness() == FULL, "a swipe wakes the screen, which stays lit");
    tapped(18 + 48, 203, "lights/main on", "开 answers after a waking swipe");

    // The page shown went away: back to the Agent page, without dots.
    reset_pages();
    send_pages();
    run(1 * SEC);
    save(out, "16-pages-gone");

    // The air conditioner: a switch, a stepper and two selects in 2x2.
    aircon("制冷 26°C · 自动风", "26°C", DOCK_ICON_COOL, "制冷", "自动");
    send_pages();
    run(1 * SEC);
    swipe(500, 250, -300, 0);
    run(1 * SEC);
    save(out, "17-aircon");
    tapped(534, 124, "aircon/temp inc", "+ sends inc");
    tapped(354, 124, "aircon/temp dec", "- sends dec");
    tapped(156, 342, "aircon/mode next", "the mode card sends next");
    tapped(444, 342, "aircon/fan next", "the fan card sends next");
    tapped(156, 146, "aircon/power toggle", "the power card toggles");

    // At the top of the range + is greyed and deaf.
    aircon("制热 30°C · 高风", "30°C", DOCK_ICON_HEAT, "制热", "高");
    s_pages.pages[0].items[1].at_max = true;
    send_pages();
    run(1 * SEC);
    save(out, "18-aircon-max");
    tapped(534, 124, NULL, "+ at the top does nothing");
    tapped(354, 124, "aircon/temp dec", "- still works at the top");

    // Dry mode holds the fan, auto the temperature.
    aircon("除湿 26°C · 低风", "26°C", DOCK_ICON_DRY, "除湿", "低");
    s_pages.pages[0].items[3].disabled = true;
    send_pages();
    run(1 * SEC);
    save(out, "19-aircon-dry");
    tapped(444, 342, NULL, "a greyed select takes no taps");
    aircon("自动 25°C · 自动风", "25°C", DOCK_ICON_AUTO, "自动", "自动");
    s_pages.pages[0].items[1].disabled = true;
    send_pages();
    run(1 * SEC);
    save(out, "20-aircon-auto");
    tapped(534, 124, NULL, "a greyed stepper takes no taps");

    // Every icon, for a look at the drawing.
    reset_pages();
    page("aircon", "图标", "", "");
    item(DOCK_ITEM_SELECT, DOCK_ICON_POWER, "a", "power", "电源");
    item(DOCK_ITEM_SELECT, DOCK_ICON_COOL, "b", "cool", "制冷");
    item(DOCK_ITEM_SELECT, DOCK_ICON_HEAT, "c", "heat", "制热");
    item(DOCK_ITEM_SELECT, DOCK_ICON_DRY, "d", "dry", "除湿");
    item(DOCK_ITEM_SELECT, DOCK_ICON_FAN, "e", "fan", "送风");
    item(DOCK_ITEM_SELECT, DOCK_ICON_AUTO, "f", "auto", "自动");
    send_pages();
    run(1 * SEC);
    save(out, "21-icons");
    reset_pages();
    page("aircon", "图标", "", "");
    item(DOCK_ITEM_SELECT, DOCK_ICON_BULB, "a", "bulb", "灯泡");
    item(DOCK_ITEM_SELECT, DOCK_ICON_MOON, "b", "moon", "变暗");
    item(DOCK_ITEM_SELECT, DOCK_ICON_BELL, "c", "bell", "提示音");
    send_pages();
    run(1 * SEC);
    save(out, "21-icons-2");
    reset_pages();
    page("aircon", "图标", "", "");
    item(DOCK_ITEM_SELECT, DOCK_ICON_CPU, "a", "cpu", "CPU");
    item(DOCK_ITEM_SELECT, DOCK_ICON_GPU, "b", "gpu", "GPU");
    item(DOCK_ITEM_SELECT, DOCK_ICON_MEMORY, "c", "memory", "内存");
    item(DOCK_ITEM_SELECT, DOCK_ICON_ROTOR, "d", "rotor", "风扇");
    item(DOCK_ITEM_SELECT, DOCK_ICON_NET, "e", "net", "网络");
    item(DOCK_ITEM_SELECT, DOCK_ICON_DISK, "f", "disk", "磁盘");
    send_pages();
    run(1 * SEC);
    save(out, "21-icons-3");

    // The 屏幕 page: brightness and dimmed brightness, when it dims and the
    // alert sound, 2x2.
    reset_pages();
    send_pages();
    run(1 * SEC);
    page("screen", "屏幕", "", "");
    item(DOCK_ITEM_STEPPER, DOCK_ICON_BULB, "bright", "亮度", "80%");
    item(DOCK_ITEM_STEPPER, DOCK_ICON_BULB, "dim", "变暗后亮度", "熄屏")->at_min = true;
    item(DOCK_ITEM_SELECT, DOCK_ICON_MOON, "dim_after", "无任务后变暗", "10 分钟");
    item(DOCK_ITEM_SWITCH, DOCK_ICON_BELL, "beep", "提示音", "已开")->on = 1;
    send_pages();
    run(1 * SEC);
    swipe(500, 250, -300, 0);
    run(1 * SEC);
    save(out, "24-screen");
    tapped(246, 124, "screen/bright inc", "+ raises the brightness");
    tapped(354, 124, NULL, "- at the bottom takes no taps");
    tapped(156, 342, "screen/dim_after next", "the delay card sends next");
    tapped(444, 342, "screen/beep toggle", "the beep card toggles");

    // The 性能 page: a dashboard of CPU and GPU big, four small cards below.
    reset_pages();
    send_pages();
    run(1 * SEC);
    perf(false);
    send_pages();
    run(1 * SEC);
    swipe(500, 250, -300, 0);
    run(1 * SEC);
    save(out, "25-perf");

    // Holding the memory card lifts it; dragged onto the GPU and let go, the
    // two swap at once and the host is told.
    int n = s_actions;
    drag(84, 368, 444, 172, 700000);
    run(200000);
    save(out, "26-perf-drag");
    release();
    check(s_actions == n + 1 && strcmp(s_action, "perf/mem swap gpu") == 0, "a card dropped on another swaps them");
    run(200000);
    save(out, "27-perf-swapped");
    // The host answers with memory big (and short of memory), the GPU small.
    perf(true);
    send_pages();
    run(1 * SEC);
    save(out, "28-perf-memory");

    // A quick swipe over a card turns the page instead; so does one that
    // moves before the card lifts. Let go anywhere else, a card goes back.
    n = s_actions;
    drag(84, 368, 384, 368, 200000);
    release();
    run(1 * SEC);
    check(s_actions == n, "a swipe over a card is not a drag");
    swipe(500, 250, -300, 0);
    run(1 * SEC);
    drag(84, 368, 300, 30, 700000);
    release();
    check(s_actions == n, "a card let go off the cards goes back");
    run(1 * SEC);
    save(out, "29-perf-back");

    // Meters without big ones sit in the grid: six of them, 3x2.
    reset_pages();
    page("perf", "性能", "", "");
    meter(DOCK_ICON_CPU, "cpu", "CPU", "96", "%", "88°C")->level = DOCK_LEVEL_CRIT;
    line_of(&s_pages.pages[0].items[0].hist, 30, 50, 1);
    meter(DOCK_ICON_GPU, "gpu", "GPU", "8", "%", "30 核");
    line_of(&s_pages.pages[0].items[1].hist, 12, 0, 5);
    meter(DOCK_ICON_MEMORY, "mem", "内存", "88", "%", "压缩 13G")->pct = 88;
    s_pages.pages[0].items[2].level = DOCK_LEVEL_WARN;
    meter(DOCK_ICON_ROTOR, "fan", "风扇", "2150", "rpm", "2 个")->pct = 31;
    meter(DOCK_ICON_NET, "net", "网络", "820", "KB/s", "↑ 1.5M");
    meter(DOCK_ICON_DISK, "disk", "磁盘", "90", "%", "剩 97G")->pct = 90;
    send_pages();
    run(1 * SEC);
    save(out, "30-perf-grid");
    tapped(108, 146, NULL, "a meter takes no taps");

    // Brightness as go-dock sets it: half, the screen off a minute after the
    // last task, a touch back to half; never dimming keeps it there.
    reset_pages();
    send_pages();
    s_settings = (settings_t){.brightness = 50, .dim_brightness = 0, .dim_after = 60};
    reset();
    add(DOCK_DONE, DOCK_CODEX, "notes", "已完成", "", 900);
    send();
    run(1 * SEC);
    check(ui_brightness() == 50 * 255 / 100, "lit at the brightness set");
    s_now_us += 2 * 60 * SEC;
    send();
    run(1 * SEC);
    check(ui_brightness() == 0, "off a minute after the last task");
    tap(300, 30);
    check(ui_brightness() == 50 * 255 / 100, "a touch lights it again");
    s_settings.dim_after = 0;
    s_now_us += 10 * 60 * SEC;
    send();
    run(1 * SEC);
    check(ui_brightness() == 50 * 255 / 100, "never dims");
    s_settings = (settings_t){.brightness = 80, .dim_brightness = 20, .dim_after = 300};
    reset_pages();
    send_pages();
    run(1 * SEC);

    // A firmware update from go-dock, then one that failed.
    s_ota = OTA_RECEIVING;
    s_ota_pct = 45;
    run(1 * SEC);
    save(out, "8-ota");
    s_ota = OTA_FAILED;
    run(1 * SEC);
    save(out, "9-ota-failed");
    s_ota = OTA_IDLE;

    // Host went away two minutes ago.
    reset();
    window(&s_snap.usage[DOCK_CLAUDE].five_hour, 62, 2 * 3600);
    add(DOCK_RUNNING, DOCK_CLAUDE, "go-dock", "进行中", "Edit: ui.c", 30);
    send();
    run(1 * SEC);
    s_now_us += 120 * SEC;
    run(2 * SEC);
    save(out, "4-offline");
    s_settings.brightness = 10;
    check(ui_brightness() == 10 * 255 / 100, "away, never brighter than the brightness set");
    return 0;
}
