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
#include "ui.h"

#define W 600
#define H 450
#define SEC 1000000LL

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

// on: 1, 0 or -1 (unknown); text NULL says what on and busy mean.
static void lamp(const char *id, const char *label, int on, bool busy, bool disabled, const char *text)
{
    dock_page_t *p = &s_pages.pages[s_pages.count - 1];
    dock_item_t *it = &p->items[p->item_count++];
    snprintf(it->id, sizeof(it->id), "%s", id);
    it->kind = DOCK_ITEM_SWITCH;
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

    // Near the limit, with an error.
    reset();
    window(&s_snap.usage[DOCK_CLAUDE].five_hour, 94, 38 * 60);
    window(&s_snap.usage[DOCK_CLAUDE].seven_day, 71, 2 * 86400);
    s_snap.usage[DOCK_CLAUDE].today_tokens = 3620000;
    window(&s_snap.usage[DOCK_CODEX].five_hour, 47, 80 * 60);
    s_snap.usage[DOCK_CODEX].today_tokens = 655000;
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

    // The assistant's latest text under the project line, as in Claude Code.
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

    // Turn clock and output tokens on the project line: running counts on
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

    // Swiping left shows the lights; tapping a card toggles it and shows it
    // switching until the host's next pages.
    swipe(500, 250, -300, 0);
    run(1 * SEC);
    save(out, "11-lights-3");
    tap(18 + 90 + 192, 244);
    check(s_actions == 1 && strcmp(s_action, "lights/desk toggle") == 0, "tap sends a toggle");
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
    check(ui_brightness() < 200, "dim after five idle minutes");
    tap(18 + 90, 54 + 92);
    check(s_actions == 1, "the waking tap toggles nothing");
    tap(18 + 90, 54 + 92);
    check(s_actions == 2 && strcmp(s_action, "lights/main toggle") == 0, "the next tap toggles");

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
    return 0;
}
