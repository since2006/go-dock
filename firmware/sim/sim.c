// Desktop renderer for the firmware UI: feeds main/ui.c sample snapshots
// and writes what the 600x450 panel would show as PPM images. ui.c is
// included, so the checks can see whose page it shows.
//
//   dock_sim <out-dir>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "../main/ui.c"

#define W 600
#define H 450
#define SEC 1000000LL
#define FULL (80 * 255 / 100) // the panel level at the default brightness

// ---- stand-ins for the ESP-IDF pieces ui.c uses ----

static int64_t s_now_us = 600 * SEC; // pretend the board has been up 10 min

int64_t esp_timer_get_time(void) { return s_now_us; }

// Go-docks on the screen, by slot, as link.c keeps them. Each one not quiet
// is heard from on every step, as their heartbeats would be.
static struct {
    link_host_t info;
    host_marks_t marks;
    dock_snapshot_t snap;
    dock_line_t line;
    uint32_t line_ver;
    int64_t rx_us;
    bool quiet;
} s_fake[LINK_HOSTS];
static uint32_t s_gen, s_ver;
static int s_cur; // the go-dock send() and send_line() send from
static dock_snapshot_t s_next; // what send() sends
static dock_line_t s_next_line;     // and send_line()

void link_hosts(link_host_t out[LINK_HOSTS])
{
    for (int i = 0; i < LINK_HOSTS; i++) {
        out[i] = s_fake[i].info;
    }
}

bool link_take_snapshot(int slot, uint32_t gen, dock_snapshot_t *out, uint32_t *version, int64_t *received_us)
{
    if (!gen || s_fake[slot].info.gen != gen || !s_fake[slot].info.version || s_fake[slot].info.version == *version) {
        return false;
    }
    memcpy(out, &s_fake[slot].snap, sizeof(*out));
    *version = s_fake[slot].info.version;
    *received_us = s_fake[slot].rx_us;
    return true;
}

bool link_take_line(int slot, uint32_t gen, dock_line_t *out, uint32_t *version)
{
    if (!gen || s_fake[slot].info.gen != gen || s_fake[slot].line_ver == *version) {
        return false;
    }
    memcpy(out, &s_fake[slot].line, sizeof(*out));
    *version = s_fake[slot].line_ver;
    return true;
}

// join: a go-dock says hello on the link in slot, which send() then uses.
static void join(int slot, const char *id, const char *name)
{
    memset(&s_fake[slot], 0, sizeof(s_fake[slot]));
    s_fake[slot].info = (link_host_t){.gen = ++s_gen, .last_rx_us = s_now_us, .top = DOCK_STATE_COUNT};
    snprintf(s_fake[slot].info.id, sizeof(s_fake[slot].info.id), "%s", id);
    snprintf(s_fake[slot].info.name, sizeof(s_fake[slot].info.name), "%s", name);
    s_fake[slot].line_ver = ++s_ver;
    s_cur = slot;
}

static void leave(int slot) { s_fake[slot].info.gen = 0; }

static wlan_status_t s_wifi;
wlan_status_t wlan_status(void) { return s_wifi; }
const char *wlan_code(void) { return "482916"; }

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
const char *ota_from(void) { return "Mac mini"; }
int ota_ask_left(void) { return 52; }
static bool s_ota_wifi;
bool ota_over_wifi(void) { return s_ota_wifi; }

// The answer tapped on the question: 1 yes, -1 no; it then shows as the
// real one would.
static int s_answer;
void ota_answer(bool yes)
{
    if (s_ota == OTA_ASKING) {
        s_answer = yes ? 1 : -1;
        s_ota = yes ? OTA_PREPARING : OTA_IDLE;
    }
}

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
    for (int i = 0; i < LINK_HOSTS; i++) {
        if (s_fake[i].info.gen && !s_fake[i].quiet) {
            s_fake[i].info.last_rx_us = s_now_us;
        }
    }
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

static void add(dock_state_t st, dock_provider_t p, const char *project, int ago)
{
    dock_session_t *s = &s_next.sessions[s_next.count++];
    snprintf(s->id, sizeof(s->id), "%s:%s", p == DOCK_CLAUDE ? "claude" : "codex", project);
    s->provider = p;
    s->state = st;
    snprintf(s->project, sizeof(s->project), "%s", project);
    s->since = NOW - ago;
    s_next.counts[st]++;
}

static dock_session_t *last(void) { return &s_next.sessions[s_next.count - 1]; }
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
    s_next.seq++;
    s_next.now = NOW;
    s_next.tz_offset = TZ;
    memcpy(&s_fake[s_cur].snap, &s_next, sizeof(s_next));
    s_fake[s_cur].info.version = ++s_ver;
    s_fake[s_cur].rx_us = s_fake[s_cur].info.last_rx_us = s_now_us;
    host_note(&s_fake[s_cur].info, &s_fake[s_cur].marks, &s_next, s_now_us);
}

static void reset(void) { memset(&s_next, 0, sizeof(s_next)); }

// A reading of the line, as the perf plugin sends it; pct -1 for none.
static void reading(dock_icon_t icon, const char *id, const char *label, const char *text, const char *unit, int pct,
                    const char *sub, dock_level_t level)
{
    dock_reading_t *r = &s_next_line.items[s_next_line.count++];
    memset(r, 0, sizeof(*r));
    r->icon = icon;
    snprintf(r->id, sizeof(r->id), "%s", id);
    snprintf(r->label, sizeof(r->label), "%s", label);
    snprintf(r->text, sizeof(r->text), "%s", text);
    snprintf(r->unit, sizeof(r->unit), "%s", unit);
    snprintf(r->sub, sizeof(r->sub), "%s", sub);
    r->pct = (int8_t)pct;
    r->level = level;
}

// A Mac's six readings: CPU, GPU, memory, then fans, the chip's
// temperature and disk.
static void mac_line(const char *cpu)
{
    memset(&s_next_line, 0, sizeof(s_next_line));
    reading(DOCK_ICON_CPU, "cpu", "CPU", cpu, "%", atoi(cpu), "", DOCK_LEVEL_OK);
    reading(DOCK_ICON_GPU, "gpu", "GPU", "57", "%", 57, "", DOCK_LEVEL_OK);
    reading(DOCK_ICON_MEMORY, "mem", "内存", "56", "%", 56, "", DOCK_LEVEL_OK);
    reading(DOCK_ICON_ROTOR, "fan", "风扇", "1000", "rpm", -1, "", DOCK_LEVEL_OK);
    reading(DOCK_ICON_TEMP, "temp", "温度", "46", "°C", -1, "", DOCK_LEVEL_OK);
    reading(DOCK_ICON_DISK, "disk", "磁盘", "40", "%", 40, "", DOCK_LEVEL_OK);
}

static void send_line(void)
{
    memcpy(&s_fake[s_cur].line, &s_next_line, sizeof(s_next_line));
    s_fake[s_cur].line_ver = ++s_ver;
    s_fake[s_cur].info.last_rx_us = s_now_us;
}

// A Mac mini's page: one task running, its readings above the usage.
static void mini_page_on(int slot)
{
    s_cur = slot;
    reset();
    window(&s_next.usage[DOCK_CLAUDE].five_hour, 41, 4 * 3600 + 33 * 60);
    window(&s_next.usage[DOCK_CLAUDE].seven_day, 18, 4 * 86400 + 14 * 3600);
    add(DOCK_RUNNING, DOCK_CLAUDE, "go-dock", 300);
    title("多台电脑轮流显示");
    detail("Edit: firmware/main/hosts.c");
    send();
    mac_line("38");
    send_line();
}

static void mini_page(void) { mini_page_on(0); }

// Several computers share the screen, a page each (hosts.c): news on one
// not shown takes over once the page shown was up 30 s, after 5 s when it
// waits for the user; without news the pages with tasks take turns every
// minute. Touching holds the page, swiping turns it, past the last
// computer to the connection page.
static void two_computers(const char *out)
{
    s_fake[0].quiet = false;
    s_now_us += 60 * SEC; // nobody touched the screen for a while
    mini_page();
    run(1 * SEC);
    check(s_shown == 0 && s_order_n == 1, "one computer, its page");

    // A MacBook comes on with an empty page: nothing new, the page stays,
    // a dot each after the name.
    join(1, "b2", "User 的 MacBook Pro");
    reset();
    window(&s_next.usage[DOCK_CLAUDE].five_hour, 12, 2 * 3600);
    send();
    s_cur = 0;
    run(1 * SEC);
    check(s_shown == 0 && s_order_n == 2, "an empty page is no news");
    save(out, "17-two-computers");

    // A task starts there: the Mac mini's page was up long enough.
    s_cur = 1;
    add(DOCK_RUNNING, DOCK_CODEX, "web-app", 5);
    title("修复登录页样式");
    detail("Bash: npm test");
    send();
    run(1 * SEC);
    check(s_shown == 1 && strcmp(s_shown_name, "User 的 MacBook Pro") == 0, "news elsewhere takes over");
    save(out, "18-other-computer");

    // A task done on the Mac mini waits until this page was up 30 s.
    mini_page();
    s_next.sessions[0].state = DOCK_DONE;
    s_next.counts[DOCK_RUNNING] = 0;
    s_next.counts[DOCK_DONE] = 1;
    s_next.sessions[0].since = NOW;
    send();
    run(20 * SEC);
    check(s_shown == 1, "not before 30 s");
    run(11 * SEC);
    check(s_shown == 0, "after 30 s");

    // A question on the MacBook, nothing waiting here: 5 s, and its dot
    // shows the colour meanwhile.
    s_cur = 1;
    reset();
    window(&s_next.usage[DOCK_CLAUDE].five_hour, 12, 2 * 3600);
    add(DOCK_APPROVAL, DOCK_CODEX, "web-app", 2);
    detail("Bash: rm -rf node_modules");
    send();
    run(3 * SEC);
    check(s_shown == 0, "even a question waits 5 s");
    save(out, "19-question-elsewhere");
    check(ui_brightness() == FULL, "a question on another computer keeps the screen lit");
    run(3 * SEC);
    check(s_shown == 1, "a question takes over after 5 s");

    // Touching holds the page: news on the Mac mini waits.
    tap(300, 300);
    mini_page();
    run(35 * SEC - 1 * SEC);
    check(s_shown == 1, "held while touched");
    // Swiping right turns to the computer before, left to the next; after
    // the last one comes the connection page, then the first again.
    s_wifi = (wlan_status_t){.state = WLAN_CONNECTED, .ssid = "home-2.4G", .ip = "192.168.31.50", .fixed = true};
    swipe(200, 300, 200, 0);
    run(1 * SEC);
    check(s_shown == 0 && !s_info, "swipe right: the computer before");
    swipe(400, 300, -200, 0);
    run(1 * SEC);
    check(s_shown == 1 && !s_info, "swipe left: the next computer");
    swipe(400, 300, -200, 0);
    run(1 * SEC);
    check(s_info, "after the last computer, the connection page");
    check(strcmp(lv_label_get_text(ui.info_addr), "192.168.31.50") == 0 &&
              strcmp(lv_label_get_text(ui.info_code), "482916") == 0,
          "the connection page shows the address and the pairing code");
    save(out, "21-connection");
    swipe(400, 300, -200, 0);
    run(1 * SEC);
    check(s_shown == 0 && !s_info, "then the first computer again");
    tap(40, 26); // the name
    run(1 * SEC);
    check(s_shown == 1 && !s_info, "tapping the name: the next page");
    tap(40, 26);
    run(1 * SEC);
    check(s_info, "tapping it again: the connection page");
    // It stays two minutes after the last touch, whatever happens meanwhile.
    run(118 * SEC);
    check(s_info, "the connection page stays while someone may read it");
    run(2 * SEC);
    check(!s_info && s_shown == 1, "then the computer's page again");
    swipe(200, 300, 200, 0);
    run(1 * SEC);
    check(s_shown == 0, "swipe right: back to the Mac mini");

    // The Mac mini's go-dock comes back on a new link: the same page.
    s_fake[0].quiet = true;
    join(2, "a1", "Mac mini");
    leave(0);
    mini_page_on(2);
    run(1 * SEC);
    check(s_shown == 2 && s_order_n == 2, "the same go-dock on a new link keeps its page");

    // The page's go-dock goes: the other page at once.
    leave(2);
    run(1 * SEC);
    check(s_shown == 1, "a page whose go-dock left gives way");

    // A long name ends in "…" before the dots.
    join(3, "c3", "一台名字特别特别长的电脑会被截断");
    s_cur = 3;
    reset();
    add(DOCK_INPUT, DOCK_CLAUDE, "notes", 3);
    detail("请选择要导出的格式");
    send();
    s_now_us += 60 * SEC;
    run(6 * SEC);
    check(s_shown == 3, "an input waiting takes over");
    save(out, "20-long-name");

    // With nothing new the pages take turns every minute: from one waiting
    // for the user only to another that waits.
    run(50 * SEC);
    check(s_shown == 3, "a page stays a minute");
    run(11 * SEC);
    check(s_shown == 1 && !s_info, "then the next page's turn");
    s_cur = 1; // the question answered: back to work, no news
    reset();
    add(DOCK_RUNNING, DOCK_CODEX, "web-app", 1);
    send();
    run(61 * SEC);
    check(s_shown == 3, "a page that waits for the user has its turn");
    run(61 * SEC);
    check(s_shown == 3, "and gives way only to another that waits");

    // A computer without tasks has no turn.
    join(2, "d4", "空闲的电脑");
    reset();
    send();
    s_cur = 3; // the input answered
    reset();
    add(DOCK_RUNNING, DOCK_CLAUDE, "notes", 1);
    send();
    run(1 * SEC); // the page was up a minute already
    check(s_shown == 1, "turns skip a page without tasks");
    run(61 * SEC);
    check(s_shown == 3, "and come back");

    // A computer joining while the connection page is up shows its page.
    for (int i = 0; i < LINK_HOSTS && !s_info; i++) {
        tap(40, 26);
        run(1 * SEC);
    }
    check(s_info, "the connection page");
    join(0, "e5", "新加的电脑");
    reset();
    send();
    run(1 * SEC);
    check(!s_info && s_shown == 0, "a computer that joins shows its page");
    s_wifi = (wlan_status_t){.state = WLAN_OFF};
}

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
    // The same with WiFi set up: joining, joined, failing.
    s_wifi = (wlan_status_t){.state = WLAN_CONNECTING, .ssid = "home-2.4G"};
    run(1 * SEC);
    save(out, "0-waiting-wifi-joining");
    s_wifi = (wlan_status_t){.state = WLAN_CONNECTED, .ssid = "home-2.4G", .ip = "192.168.31.50", .fixed = true};
    run(1 * SEC);
    save(out, "0-waiting-wifi");
    s_wifi = (wlan_status_t){.state = WLAN_FAILED, .ssid = "home-2.4G", .err = "密码不对"};
    run(1 * SEC);
    save(out, "0-waiting-wifi-failed");
    s_wifi = (wlan_status_t){.state = WLAN_OFF};

    // Everyday mix, two pages of tasks.
    join(0, "a1", "");
    reset();
    window(&s_next.usage[DOCK_CLAUDE].five_hour, 62, 4 * 3600 + 33 * 60);
    window(&s_next.usage[DOCK_CLAUDE].seven_day, 31, 4 * 86400 + 14 * 3600);
    s_next.usage[DOCK_CLAUDE].today_tokens = 1840000;
    window(&s_next.usage[DOCK_CODEX].five_hour, 28, 3 * 3600 + 5 * 60);
    window(&s_next.usage[DOCK_CODEX].seven_day, 12, 5 * 86400);
    s_next.usage[DOCK_CODEX].today_tokens = 402000;
    add(DOCK_APPROVAL, DOCK_CLAUDE, "go-dock", 42);
    agent(DOCK_APPROVAL, "Explore", "Bash: cat ~/.ssh/config", 18);
    agent(DOCK_RUNNING, "Explore", "Explore host tree toolbar & store", 186);
    agent(DOCK_RUNNING, "Explore", "Explore tables, dialogs, checkboxes", 178);
    detail("AskUserQuestion");
    add(DOCK_INPUT, DOCK_CLAUDE, "infra", 65);
    detail("请选择部署区域");
    add(DOCK_RUNNING, DOCK_CODEX, "api-svc", 192);
    title("重构登录模块，拆分 token 校验");
    msg("先跑一遍认证相关的测试，看哪些用例失败。");
    detail("Bash: pytest -q tests/auth");
    add(DOCK_RUNNING, DOCK_CLAUDE, "另一个项目", 45);
    detail("Edit: launch.md");
    add(DOCK_DONE, DOCK_CODEX, "scripts", 120);
    msg("迁移脚本已完成，共更新 3 张表。");
    add(DOCK_DONE, DOCK_CLAUDE, "dotfiles", 540);
    detail("整理 zsh 配置");
    send();
    run(2 * SEC);
    save(out, "1-mixed");

    // Near the limit, with an error; Codex shows the widest usage texts.
    reset();
    window(&s_next.usage[DOCK_CLAUDE].five_hour, 94, 38 * 60);
    window(&s_next.usage[DOCK_CLAUDE].seven_day, 71, 2 * 86400);
    s_next.usage[DOCK_CLAUDE].today_tokens = 3620000;
    window(&s_next.usage[DOCK_CODEX].five_hour, 100, 4 * 3600 + 33 * 60);
    window(&s_next.usage[DOCK_CODEX].seven_day, 100, 4 * 86400 + 14 * 3600);
    s_next.usage[DOCK_CODEX].today_tokens = 12300000;
    add(DOCK_ERROR, DOCK_CLAUDE, "go-dock", 62);
    detail("额度已用完，0:38 后恢复");
    add(DOCK_APPROVAL, DOCK_CODEX, "api-svc", 15);
    detail("Bash: rm -rf dist");
    add(DOCK_RUNNING, DOCK_CODEX, "a-very-long-project-name-that-overflows", 288);
    detail("Edit: src/components/navigation/HeaderWithAVeryLongName.tsx");
    agent(DOCK_RUNNING, "general-purpose", "迁移导航组件到新版路由", 120);
    agent(DOCK_DONE, "Explore", "查找旧路由用法", 300);
    send();
    run(2 * SEC);
    save(out, "2-limit");

    // Nothing running.
    reset();
    window(&s_next.usage[DOCK_CLAUDE].five_hour, 8, 4 * 3600 + 41 * 60);
    window(&s_next.usage[DOCK_CLAUDE].seven_day, 22, 4 * 86400);
    s_next.usage[DOCK_CLAUDE].today_tokens = 96000;
    send();
    run(2 * SEC);
    save(out, "3-empty");

    // Main turn ended while background subagents still work.
    reset();
    window(&s_next.usage[DOCK_CLAUDE].five_hour, 41, 3 * 3600 + 20 * 60);
    window(&s_next.usage[DOCK_CLAUDE].seven_day, 18, 4 * 86400 + 15 * 3600);
    add(DOCK_RUNNING, DOCK_CLAUDE, "host-app", 18);
    title("规划 host 端工具栏");
    msg("派两个 Explore 子任务分别查看工具栏和表格组件。");
    detail("等待 2 个子任务完成");
    agent(DOCK_RUNNING, "Explore", "Explore toolbar & store", 18);
    agent(DOCK_RUNNING, "Explore", "Explore tables, dialogs", 18);
    add(DOCK_DONE, DOCK_CODEX, "scripts", 300);
    detail("迁移数据库脚本");
    send();
    run(2 * SEC);
    save(out, "5-background");

    // Under the project line what the session is doing, else the
    // assistant's latest text.
    reset();
    window(&s_next.usage[DOCK_CLAUDE].five_hour, 41, 4 * 3600 + 33 * 60);
    window(&s_next.usage[DOCK_CLAUDE].seven_day, 18, 4 * 86400 + 14 * 3600);
    add(DOCK_RUNNING, DOCK_CLAUDE, "shellrs", 114);
    title("detail");
    msg("先把两张截图的像素颜色取出来，对比 ShellRS 的配色和渲染。");
    detail("Finding the red colon's color in both screenshots");
    add(DOCK_RUNNING, DOCK_CLAUDE, "a-very-long-project-name", 30);
    detail("Read: ui.c");
    title("一个很长很长的会话名字会被截断");
    agent(DOCK_RUNNING, "Explore", "查找字体渲染代码", 20);
    add(DOCK_DONE, DOCK_CODEX, "scripts", 300);
    title("迁移数据库脚本");
    msg("迁移脚本已完成，共更新 3 张表。");
    send();
    run(2 * SEC);
    save(out, "6-message");

    // Two sessions fill a page; swiping up shows the next two. Symbols
    // beyond GB2312's hanzi (§, •, –, ≥, ✓) have glyphs.
    reset();
    window(&s_next.usage[DOCK_CLAUDE].five_hour, 41, 4 * 3600 + 33 * 60);
    window(&s_next.usage[DOCK_CLAUDE].seven_day, 18, 4 * 86400 + 14 * 3600);
    add(DOCK_RUNNING, DOCK_CLAUDE, "go-dock", 95);
    title("硬件刷机");
    turn(95, 6500);
    detail("Bash: idf.py build");
    add(DOCK_DONE, DOCK_CLAUDE, "host-app", 300);
    title("整理布局文档");
    msg("§7.1 的布局说明已更新 • 行高 126px");
    add(DOCK_DONE, DOCK_CODEX, "scripts", 900);
    msg("迁移完成 ✓ 3 张表 – 耗时 ≥ 2 分钟");
    add(DOCK_DONE, DOCK_CLAUDE, "dotfiles", 3600);
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
    window(&s_next.usage[DOCK_CLAUDE].five_hour, 41, 4 * 3600 + 33 * 60);
    window(&s_next.usage[DOCK_CLAUDE].seven_day, 18, 4 * 86400 + 14 * 3600);
    add(DOCK_APPROVAL, DOCK_CLAUDE, "api-svc", 15);
    turn(42, 1200);
    detail("Bash: rm -rf dist");
    add(DOCK_RUNNING, DOCK_CLAUDE, "shellrs", 125);
    title("detail");
    turn(125, 6500);
    msg("先把两张截图的像素颜色取出来，对比 ShellRS 和 iTerm2 的配色、字体渲染和行高。");
    detail("Finding the red colon's color in both screenshots and comparing them");
    add(DOCK_DONE, DOCK_CODEX, "notes", 900);
    send();
    run(2 * SEC);
    save(out, "7-turn");

    // The line of readings above the usage, three at most: they share it
    // evenly, the usage card tightens and the tasks stay where they were.
    // The strip is titled with the computer's name.
    snprintf(s_fake[0].info.name, sizeof(s_fake[0].info.name), "Mac mini");
    memset(&s_next_line, 0, sizeof(s_next_line));
    reading(DOCK_ICON_CPU, "cpu", "CPU", "95", "%", 95, "", DOCK_LEVEL_CRIT);
    reading(DOCK_ICON_MEMORY, "mem", "内存", "88", "%", 88, "", DOCK_LEVEL_WARN);
    reading(DOCK_ICON_TEMP, "temp", "温度", "72", "°C", -1, "", DOCK_LEVEL_OK);
    send();
    send_line();
    run(1 * SEC);
    save(out, "10-line-three");

    // Six take turns, three at a time: the first group stays six seconds,
    // then the next slides in from the right over half a second. New
    // readings in between change the numbers, not the turning.
    mac_line("38");
    send_line();
    run(1 * SEC);
    save(out, "11-line");
    run(2 * SEC);
    mac_line("41");
    send_line();
    run(3 * SEC - 50000 + 250000); // halfway through the slide
    save(out, "12-line-sliding");
    run(1 * SEC);
    save(out, "13-line-next");
    run(6 * SEC);
    save(out, "14-line-again"); // after the last group, the first slides in again

    // Five on a computer without fans: the second group has two.
    memset(&s_next_line, 0, sizeof(s_next_line));
    reading(DOCK_ICON_CPU, "cpu", "CPU", "100", "%", 100, "", DOCK_LEVEL_CRIT);
    reading(DOCK_ICON_GPU, "gpu", "GPU", "78", "%", 78, "", DOCK_LEVEL_WARN);
    reading(DOCK_ICON_MEMORY, "mem", "内存", "41", "%", 41, "", DOCK_LEVEL_OK);
    reading(DOCK_ICON_TEMP, "temp", "温度", "88", "°C", -1, "", DOCK_LEVEL_WARN);
    reading(DOCK_ICON_DISK, "disk", "磁盘", "97", "%", 97, "", DOCK_LEVEL_CRIT);
    send_line();
    run(7 * SEC);
    save(out, "15-line-five");

    // The plugin turned off: the Agent page as before.
    memset(&s_next_line, 0, sizeof(s_next_line));
    send_line();
    run(1 * SEC);
    save(out, "16-line-gone");

    // A screen dimmed for want of activity; a touch wakes it.
    reset();
    add(DOCK_DONE, DOCK_CODEX, "notes", 900);
    send();
    s_now_us += 6 * 60 * SEC;
    send();
    run(1 * SEC);
    check(ui_brightness() == 20 * 255 / 100, "dim after five idle minutes");
    tap(300, 300);
    check(ui_brightness() == FULL, "a touch wakes it");

    // Brightness as go-dock sets it: half, the screen off a minute after the
    // last task, a touch back to half; never dimming keeps it there.
    s_settings = (settings_t){.brightness = 50, .dim_brightness = 0, .dim_after = 60};
    reset();
    add(DOCK_DONE, DOCK_CODEX, "notes", 900);
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
    run(1 * SEC);

    // A firmware update from go-dock over USB, then one that failed.
    s_ota = OTA_RECEIVING;
    s_ota_pct = 45;
    run(1 * SEC);
    save(out, "8-ota");
    s_ota = OTA_FAILED;
    run(1 * SEC);
    save(out, "9-ota-failed");

    // Over WiFi the screen asks first, lit up even if it was dim, and only a
    // tap on 升级 says yes; 取消 says no.
    s_ota_wifi = true;
    s_now_us += 6 * 60 * SEC;
    s_ota = OTA_ASKING;
    run(1 * SEC);
    save(out, "8-ota-ask");
    check(ui_brightness() == FULL, "the question lights the screen");
    tap(300, 200); // on the card, not on an answer
    check(s_answer == 0 && s_ota == OTA_ASKING, "a tap beside the answers says nothing");
    tap(383, 305);
    check(s_answer == 1, "升级 says yes");
    run(1 * SEC);
    save(out, "8-ota-yes");
    s_answer = 0;
    s_ota = OTA_ASKING;
    run(1 * SEC);
    tap(217, 305);
    check(s_answer == -1 && s_ota == OTA_IDLE, "取消 says no");
    s_ota = OTA_RECEIVING;
    run(1 * SEC);
    save(out, "8-ota-wifi"); // the power must stay on, not the cable
    s_ota = OTA_IDLE;
    s_ota_wifi = false;

    // Host went away two minutes ago.
    reset();
    window(&s_next.usage[DOCK_CLAUDE].five_hour, 62, 2 * 3600);
    add(DOCK_RUNNING, DOCK_CLAUDE, "go-dock", 30);
    detail("Edit: ui.c");
    send();
    run(1 * SEC);
    s_fake[0].quiet = true;
    s_now_us += 120 * SEC;
    run(2 * SEC);
    save(out, "4-offline");
    // The same go-dock was on WiFi: the card says where to reach the screen,
    // or what is wrong with the WiFi.
    s_wifi = (wlan_status_t){.state = WLAN_CONNECTED, .ssid = "home-2.4G", .ip = "192.168.31.50"};
    run(1 * SEC);
    save(out, "4-offline-wifi");
    s_wifi = (wlan_status_t){.state = WLAN_CONNECTING, .ssid = "home-2.4G"};
    run(1 * SEC);
    save(out, "4-offline-wifi-joining");
    s_wifi = (wlan_status_t){.state = WLAN_OFF};
    s_settings.brightness = 10;
    check(ui_brightness() == 10 * 255 / 100, "away, never brighter than the brightness set");
    s_settings.brightness = 80;

    two_computers(out);
    return 0;
}
