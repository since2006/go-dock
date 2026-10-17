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

static void run(int64_t us)
{
    for (int64_t t = 0; t < us; t += 50000) {
        s_now_us += 50000;
        ui_poll();
        lv_timer_handler();
    }
    lv_refr_now(NULL);
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

int main(int argc, char **argv)
{
    const char *out = argc > 1 ? argv[1] : ".";
    lv_init();
    lv_tick_set_cb(tick);
    lv_display_t *disp = lv_display_create(W, H);
    static uint16_t buf[W * 60];
    lv_display_set_buffers(disp, buf, NULL, sizeof(buf), LV_DISPLAY_RENDER_MODE_PARTIAL);
    lv_display_set_flush_cb(disp, flush);
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
