#include "link.h"

#include <errno.h>
#include <stdarg.h>
#include <stdio.h>
#include <string.h>

#include "beep.h"
#include "cJSON.h"
#include "driver/usb_serial_jtag.h"
#include "esp_app_desc.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "lwip/sockets.h"
#include "ota.h"
#include "settings.h"
#include "wlan.h"

#define MAX_LINE 4096
#define NET_CONNS 4                     // TCP links at once
#define NET_HELLO_US (5 * 1000000LL)    // a TCP link must say hello with the pairing code by then
#define NET_SILENT_US (20 * 1000000LL)  // a TCP link quiet this long is dropped
#define NET_DENY_US (2 * 1000000LL)     // a link turned away is closed this much later
#define HOST_GONE_US (15 * 1000000LL)   // the go-dock on the screen counts as gone after this

static const char *TAG = "link";

typedef enum {
    CONN_USB,
    CONN_NET,
} conn_kind_t;

// One link to a go-dock. Its reader (the USB task or the TCP task) owns
// the line buffer; the rest changes under s_rx_lock, and fd and broken
// under s_tx_lock as well.
typedef struct {
    conn_kind_t kind;
    int fd;             // CONN_NET: the socket, -1 when the slot is free
    bool broken;        // CONN_NET: a write failed; the TCP task closes it
    bool authed;        // CONN_NET: said hello with the pairing code
    int64_t opened_us;  // CONN_NET: accepted at
    uint32_t peer;      // CONN_NET: its IPv4 address
    int64_t last_rx_us; // last message handled, 0 none yet
    int64_t close_us;   // CONN_NET: turned away, closed at this time (0: not)
    char name[52];      // the computer's name (12 runes), from its hello
    char id[17];        // and its id
    char *line;
    size_t len;
    bool skipping; // inside a line longer than MAX_LINE
} conn_t;

static SemaphoreHandle_t s_rx_lock; // one message handled at a time; guards s_owner and the links
static SemaphoreHandle_t s_tx_lock; // keeps lines whole; guards writes, fds and s_owner
static SemaphoreHandle_t s_lock;    // guards s_shared, s_version, s_received_us, the line and s_host_name
static conn_t s_usb;
static conn_t s_net[NET_CONNS];
// The go-dock whose state the screen shows, always on the network; only
// its messages count. The first one keeps it until it goes quiet, unless
// the same go-dock (same id) comes back on a new link. The USB cable only
// sets up the WiFi and updates the firmware.
static conn_t *s_owner;
// After a wrong pairing code, that address gets no new link before
// s_net_wait_us; others, the real go-dock among them, still do.
static int64_t s_net_wait_us;
static uint32_t s_net_wait_peer;
static dock_snapshot_t *s_shared;    // newest complete snapshot
static dock_snapshot_t *s_scratch;   // parse target, under s_rx_lock
static uint32_t s_version;
static dock_line_t *s_line;         // newest line of readings
static dock_line_t *s_line_scratch; // parse target, under s_rx_lock
static uint32_t s_line_version;
static char s_host_name[52]; // the name of the go-dock the screen shows, from its hello
static int64_t s_received_us;
static volatile int64_t s_last_rx_us;
static volatile bool s_incompatible;
static conn_t *s_ota_conn;  // the link the last update (or its question) came over, under s_rx_lock

static bool net_send(int fd, const char *buf, size_t n, int flags)
{
    while (n > 0) {
        int r = send(fd, buf, n, flags);
        if (r <= 0) {
            return false;
        }
        buf += r;
        n -= (size_t)r;
    }
    return true;
}

// conn_write writes one line to c; the caller holds s_tx_lock.
static void conn_write(conn_t *c, const char *json)
{
    if (c->kind == CONN_USB) {
        // Short timeouts: with no host attached the write would only block.
        usb_serial_jtag_write_bytes(json, strlen(json), pdMS_TO_TICKS(50));
        usb_serial_jtag_write_bytes("\n", 1, pdMS_TO_TICKS(50));
    } else if (c->fd >= 0 && !c->broken) {
        if (!net_send(c->fd, json, strlen(json), MSG_MORE) || !net_send(c->fd, "\n", 1, 0)) {
            c->broken = true;
        }
    }
}

static void send_to(conn_t *c, const char *json)
{
    xSemaphoreTake(s_tx_lock, portMAX_DELAY);
    conn_write(c, json);
    xSemaphoreGive(s_tx_lock);
}

// send_host sends to the go-dock the screen shows, if any.
static void send_host(const char *json)
{
    xSemaphoreTake(s_tx_lock, portMAX_DELAY);
    if (s_owner) {
        conn_write(s_owner, json);
    }
    xSemaphoreGive(s_tx_lock);
}

static void set_owner(conn_t *c)
{
    xSemaphoreTake(s_tx_lock, portMAX_DELAY);
    s_owner = c;
    xSemaphoreGive(s_tx_lock);
}

static void send_hello(conn_t *c)
{
    char buf[120];
    snprintf(buf, sizeof(buf), "{\"t\":\"hello\",\"proto\":%d,\"fw\":\"%s\",\"w\":600,\"h\":450}", DOCK_PROTO,
             esp_app_get_description()->version);
    send_to(c, buf);
}

// set_host_name keeps the name of the go-dock the screen shows, for its
// title.
static void set_host_name(const char *name)
{
    xSemaphoreTake(s_lock, portMAX_DELAY);
    snprintf(s_host_name, sizeof(s_host_name), "%s", name);
    xSemaphoreGive(s_lock);
}

// send_log sends a log line to c, or to the go-dock on the screen when c
// is NULL.
static void send_log(conn_t *c, const char *level, const char *msg)
{
    ESP_LOGI(TAG, "%s", msg);
    cJSON *o = cJSON_CreateObject();
    cJSON_AddStringToObject(o, "t", "log");
    cJSON_AddStringToObject(o, "level", level);
    cJSON_AddStringToObject(o, "msg", msg);
    char *line = cJSON_PrintUnformatted(o);
    if (line) {
        if (c) {
            send_to(c, line);
        } else {
            send_host(line);
        }
        cJSON_free(line);
    }
    cJSON_Delete(o);
}

void link_log(const char *level, const char *fmt, ...)
{
    char msg[200];
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(msg, sizeof(msg), fmt, ap);
    va_end(ap);
    send_log(NULL, level, msg);
}

// send_net tells a go-dock on USB how the WiFi is doing (§4.5).
static void send_net(conn_t *c)
{
    static const char *const states[] = {"off", "connecting", "connected", "failed"};
    if (!wlan_ready()) {
        return; // no pairing code yet; sent once WiFi starts (wlan_version)
    }
    wlan_status_t st = wlan_status();
    cJSON *o = cJSON_CreateObject();
    cJSON_AddStringToObject(o, "t", "net");
    cJSON_AddStringToObject(o, "state", states[st.state]);
    if (st.ssid[0]) {
        cJSON_AddStringToObject(o, "ssid", st.ssid);
    }
    if (st.ip[0]) {
        cJSON_AddStringToObject(o, "ip", st.ip);
    }
    cJSON_AddNumberToObject(o, "port", WLAN_PORT);
    cJSON_AddStringToObject(o, "code", wlan_code());
    if (st.fixed) {
        cJSON_AddTrueToObject(o, "fixed");
    }
    if (st.err[0]) {
        cJSON_AddStringToObject(o, "err", st.err);
    }
    char *line = cJSON_PrintUnformatted(o);
    if (line) {
        send_to(c, line);
        cJSON_free(line);
    }
    cJSON_Delete(o);
}

// send_scan reports the networks a scan found (§4.6), or why it failed.
static void send_scan(conn_t *c, const wlan_scan_t *scan, const char *err)
{
    cJSON *o = cJSON_CreateObject();
    cJSON_AddStringToObject(o, "t", "wifi_list");
    cJSON *aps = cJSON_AddArrayToObject(o, "aps");
    for (int i = 0; scan && i < scan->count; i++) {
        cJSON *ap = cJSON_CreateObject();
        cJSON_AddStringToObject(ap, "ssid", scan->aps[i].ssid);
        cJSON_AddNumberToObject(ap, "rssi", scan->aps[i].rssi);
        if (scan->aps[i].open) {
            cJSON_AddTrueToObject(ap, "open");
        }
        cJSON_AddItemToArray(aps, ap);
    }
    if (err && err[0]) {
        cJSON_AddStringToObject(o, "err", err);
    }
    // Names full of characters JSON escapes could make the line too long:
    // the weakest go first.
    char *line = cJSON_PrintUnformatted(o);
    while (line && strlen(line) >= MAX_LINE && cJSON_GetArraySize(aps) > 0) {
        cJSON_free(line);
        cJSON_DeleteItemFromArray(aps, cJSON_GetArraySize(aps) - 1);
        line = cJSON_PrintUnformatted(o);
    }
    if (line) {
        send_to(c, line);
        cJSON_free(line);
    }
    cJSON_Delete(o);
}

// deny turns a TCP link away (§4.7): it hears why, and is closed a little
// later so the reason gets there first.
static void deny(conn_t *c, const char *msg, int64_t now)
{
    cJSON *o = cJSON_CreateObject();
    cJSON_AddStringToObject(o, "t", "denied");
    cJSON_AddStringToObject(o, "msg", msg);
    char *line = cJSON_PrintUnformatted(o);
    if (line) {
        send_to(c, line);
        cJSON_free(line);
    }
    cJSON_Delete(o);
    c->close_us = now + NET_DENY_US;
    if (s_owner == c) {
        set_owner(NULL);
    }
    ESP_LOGI(TAG, "turned away %s: %s", c->name[0] ? c->name : "a go-dock", msg);
}

static bool conn_open(const conn_t *c)
{
    return c->kind == CONN_USB || (c->fd >= 0 && !c->close_us && !c->broken);
}

// take_over gives the screen to c; the link it takes the screen from (the
// same go-dock on a link it left behind) is turned away.
static void take_over(conn_t *c, int64_t now)
{
    conn_t *o = s_owner;
    if (o && o != c && conn_open(o)) {
        deny(o, "这台电脑从新的连接连上了屏幕", now);
    }
    c->last_rx_us = now;
    set_owner(c);
    set_host_name(c->name);
}

static void copy_str(char *dst, size_t n, const cJSON *v)
{
    const char *s = cJSON_IsString(v) ? v->valuestring : "";
    snprintf(dst, n, "%s", s);
}

static int64_t num(const cJSON *v)
{
    return cJSON_IsNumber(v) ? (int64_t)v->valuedouble : 0;
}

static dock_state_t parse_state(const char *s)
{
    static const char *names[] = {"approval", "input", "error", "running", "done", "idle"};
    for (int i = 0; i < DOCK_STATE_COUNT; i++) {
        if (s && strcmp(s, names[i]) == 0) {
            return (dock_state_t)i;
        }
    }
    return DOCK_IDLE;
}

static void parse_window(const cJSON *w, dock_window_t *out)
{
    memset(out, 0, sizeof(*out));
    if (!cJSON_IsObject(w)) {
        return; // null: no data for this window
    }
    out->present = true;
    const cJSON *pct = cJSON_GetObjectItem(w, "pct");
    if (cJSON_IsNumber(pct)) {
        out->pct = (float)pct->valuedouble;
    }
    out->resets_at = num(cJSON_GetObjectItem(w, "resets_at"));
}

static void parse_usage(const cJSON *u, dock_usage_t *out)
{
    memset(out, 0, sizeof(*out));
    parse_window(cJSON_GetObjectItem(u, "five_hour"), &out->five_hour);
    parse_window(cJSON_GetObjectItem(u, "seven_day"), &out->seven_day);
    out->today_tokens = num(cJSON_GetObjectItem(u, "today_tokens"));
}

static void parse_snapshot(const cJSON *root, dock_snapshot_t *s)
{
    memset(s, 0, sizeof(*s));
    s->seq = (uint32_t)num(cJSON_GetObjectItem(root, "seq"));
    s->now = num(cJSON_GetObjectItem(root, "now"));
    s->tz_offset = (int32_t)num(cJSON_GetObjectItem(root, "tz"));
    const cJSON *usage = cJSON_GetObjectItem(root, "usage");
    parse_usage(cJSON_GetObjectItem(usage, "claude"), &s->usage[DOCK_CLAUDE]);
    parse_usage(cJSON_GetObjectItem(usage, "codex"), &s->usage[DOCK_CODEX]);

    const cJSON *item;
    cJSON_ArrayForEach(item, cJSON_GetObjectItem(root, "sessions"))
    {
        if (s->count >= DOCK_MAX_SESSIONS) {
            break;
        }
        dock_session_t *d = &s->sessions[s->count++];
        copy_str(d->id, sizeof(d->id), cJSON_GetObjectItem(item, "id"));
        const cJSON *p = cJSON_GetObjectItem(item, "p");
        d->provider = (cJSON_IsString(p) && strcmp(p->valuestring, "codex") == 0) ? DOCK_CODEX : DOCK_CLAUDE;
        const cJSON *st = cJSON_GetObjectItem(item, "state");
        d->state = parse_state(cJSON_IsString(st) ? st->valuestring : NULL);
        copy_str(d->project, sizeof(d->project), cJSON_GetObjectItem(item, "project"));
        d->since = num(cJSON_GetObjectItem(item, "since"));
        copy_str(d->title, sizeof(d->title), cJSON_GetObjectItem(item, "title"));
        copy_str(d->msg, sizeof(d->msg), cJSON_GetObjectItem(item, "msg"));
        copy_str(d->detail, sizeof(d->detail), cJSON_GetObjectItem(item, "detail"));
        d->turn_output = num(cJSON_GetObjectItem(item, "turn_output"));
        d->turn_secs = num(cJSON_GetObjectItem(item, "turn_secs"));
        d->agents_total = (int)num(cJSON_GetObjectItem(item, "agents_total"));
        d->agents_active = (int)num(cJSON_GetObjectItem(item, "agents_active"));
        const cJSON *ag;
        cJSON_ArrayForEach(ag, cJSON_GetObjectItem(item, "agents"))
        {
            if (d->agent_count >= DOCK_MAX_AGENTS) {
                break;
            }
            dock_agent_t *a = &d->agents[d->agent_count++];
            copy_str(a->type, sizeof(a->type), cJSON_GetObjectItem(ag, "type"));
            copy_str(a->text, sizeof(a->text), cJSON_GetObjectItem(ag, "text"));
            const cJSON *ast = cJSON_GetObjectItem(ag, "state");
            a->state = parse_state(cJSON_IsString(ast) ? ast->valuestring : NULL);
            a->since = num(cJSON_GetObjectItem(ag, "since"));
        }
    }

    const cJSON *counts = cJSON_GetObjectItem(root, "counts");
    static const char *names[] = {"approval", "input", "error", "running", "done", "idle"};
    for (int i = 0; i < DOCK_STATE_COUNT; i++) {
        s->counts[i] = (int)num(cJSON_GetObjectItem(counts, names[i]));
    }
}

// Copies a reading's id; only [a-z0-9_-]{1,16} passes.
static bool copy_id(char *dst, const cJSON *v)
{
    const char *s = cJSON_IsString(v) ? v->valuestring : "";
    size_t n = strlen(s);
    if (n == 0 || n >= DOCK_ID_LEN) {
        return false;
    }
    for (size_t i = 0; i < n; i++) {
        char c = s[i];
        if (!((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_')) {
            return false;
        }
    }
    memcpy(dst, s, n + 1);
    return true;
}

static int name_index(const cJSON *v, const char *const *names, int n)
{
    for (int i = 0; cJSON_IsString(v) && i < n; i++) {
        if (strcmp(v->valuestring, names[i]) == 0) {
            return i;
        }
    }
    return -1;
}

// An icon this firmware does not know is none.
static dock_icon_t parse_icon(const cJSON *v)
{
    static const char *const names[DOCK_ICON_COUNT - 1] = {"cpu", "gpu", "memory", "rotor", "net", "disk"};
    return (dock_icon_t)(name_index(v, names, DOCK_ICON_COUNT - 1) + 1);
}

// line: the readings above the usage, replacing the ones before. Readings
// with ids the device could not echo back are left out.
static void parse_line(const cJSON *root, dock_line_t *out)
{
    static const char *const levels[] = {"warn", "crit"};
    memset(out, 0, sizeof(*out));
    const cJSON *it;
    cJSON_ArrayForEach(it, cJSON_GetObjectItem(root, "items"))
    {
        if (out->count >= DOCK_MAX_READINGS) {
            break;
        }
        dock_reading_t *r = &out->items[out->count];
        if (!copy_id(r->id, cJSON_GetObjectItem(it, "id"))) {
            continue;
        }
        out->count++;
        r->icon = parse_icon(cJSON_GetObjectItem(it, "icon"));
        copy_str(r->label, sizeof(r->label), cJSON_GetObjectItem(it, "label"));
        copy_str(r->text, sizeof(r->text), cJSON_GetObjectItem(it, "text"));
        copy_str(r->unit, sizeof(r->unit), cJSON_GetObjectItem(it, "unit"));
        copy_str(r->sub, sizeof(r->sub), cJSON_GetObjectItem(it, "sub"));
        const cJSON *pct = cJSON_GetObjectItem(it, "pct");
        int64_t p = num(pct);
        r->pct = cJSON_IsNumber(pct) ? (int8_t)(p < 0 ? 0 : p > 100 ? 100 : p) : -1;
        r->level = (dock_level_t)(name_index(cJSON_GetObjectItem(it, "level"), levels, 2) + 1);
    }
}

// A number field cut to lo..hi; def when it is missing.
static int64_t num_in(const cJSON *v, int64_t lo, int64_t hi, int64_t def)
{
    if (!cJSON_IsNumber(v)) {
        return def;
    }
    int64_t n = num(v);
    return n < lo ? lo : n > hi ? hi : n;
}

// settings: the screen's brightness; a field left out keeps its value.
static void handle_settings(const cJSON *root)
{
    settings_t s = settings_get();
    s.brightness = (uint8_t)num_in(cJSON_GetObjectItem(root, "brightness"), 10, 100, s.brightness);
    s.dim_brightness = (uint8_t)num_in(cJSON_GetObjectItem(root, "dim_brightness"), 0, 100, s.dim_brightness);
    s.dim_after = (uint32_t)num_in(cJSON_GetObjectItem(root, "dim_after"), 0, 24 * 3600, s.dim_after);
    if (!settings_set(s)) {
        return;
    }
    if (s.dim_after) {
        link_log("info", "屏幕亮度 %u%%，无任务 %lu 秒后降到 %u%%", s.brightness, (unsigned long)s.dim_after,
                 s.dim_brightness);
    } else {
        link_log("info", "屏幕亮度 %u%%，不自动变暗", s.brightness);
    }
}

// beep: {"notes":[[hz,ms],...],"vol":percent}; a frequency out of range
// plays as a rest, no vol plays at full volume.
static void handle_beep(const cJSON *root)
{
    static beep_note_t notes[BEEP_MAX_NOTES]; // under s_rx_lock
    int n = 0;
    const cJSON *note;
    cJSON_ArrayForEach(note, cJSON_GetObjectItem(root, "notes"))
    {
        if (n == BEEP_MAX_NOTES) {
            break;
        }
        int64_t hz = num(cJSON_GetArrayItem(note, 0));
        int64_t ms = num(cJSON_GetArrayItem(note, 1));
        if (ms <= 0) {
            continue;
        }
        notes[n].hz = (hz >= 100 && hz <= 8000) ? (uint16_t)hz : 0;
        notes[n].ms = ms > 1000 ? 1000 : (uint16_t)ms;
        n++;
    }
    beep_play(notes, n, (int)num_in(cJSON_GetObjectItem(root, "vol"), 1, 100, 100));
}

static void send_ota_error(conn_t *c, const char *err)
{
    cJSON *o = cJSON_CreateObject();
    cJSON_AddStringToObject(o, "t", "ota");
    cJSON_AddStringToObject(o, "state", "error");
    cJSON_AddStringToObject(o, "msg", err);
    char *line = cJSON_PrintUnformatted(o);
    if (line) {
        send_to(c, line);
        cJSON_free(line);
    }
    cJSON_Delete(o);
}

// Firmware update messages work whatever the protocol version: they are
// how a device too old for its host gets fixed. Over USB an update starts
// at once; over WiFi the screen asks the user first (confirm), and the
// net task carries on once they answered (net_ota). One link at a time.
static void handle_ota(conn_t *c, const char *t, const cJSON *root)
{
    char reply[64];
    const char *err;
    if (strcmp(t, "ota_begin") == 0) {
        if (ota_busy() && s_ota_conn != c) {
            send_ota_error(c, "屏幕正在经另一条连接升级固件");
            return;
        }
        s_ota_conn = c;
        const cJSON *ver = cJSON_GetObjectItem(root, "ver");
        size_t size = (size_t)num(cJSON_GetObjectItem(root, "size"));
        const char *v = cJSON_IsString(ver) ? ver->valuestring : "";
        if (c->kind == CONN_NET) {
            err = ota_ask(size, v, c->name);
            snprintf(reply, sizeof(reply), "{\"t\":\"ota\",\"state\":\"confirm\"}");
        } else {
            err = ota_begin(size, v);
            snprintf(reply, sizeof(reply), "{\"t\":\"ota\",\"state\":\"ready\"}");
        }
    } else if (c != s_ota_conn) {
        if (strcmp(t, "ota_abort") != 0) {
            send_ota_error(c, "没有进行中的升级");
        }
        return; // another link's update goes on
    } else if (strcmp(t, "ota_data") == 0) {
        const cJSON *data = cJSON_GetObjectItem(root, "data");
        size_t written = 0;
        err = ota_write((size_t)num(cJSON_GetObjectItem(root, "off")), cJSON_IsString(data) ? data->valuestring : "",
                        &written);
        snprintf(reply, sizeof(reply), "{\"t\":\"ota\",\"state\":\"ack\",\"off\":%u}", (unsigned)written);
    } else if (strcmp(t, "ota_end") == 0) {
        err = ota_finish();
        if (!err) {
            send_to(c, "{\"t\":\"ota\",\"state\":\"done\"}");
            if (c->kind == CONN_USB) {
                usb_serial_jtag_wait_tx_done(pdMS_TO_TICKS(200));
            }
            vTaskDelay(pdMS_TO_TICKS(1000)); // the screen shows the restart; TCP sends the answer
            esp_restart();
        }
    } else if (strcmp(t, "ota_abort") == 0) {
        ota_abort("go-dock 取消了升级");
        return;
    } else {
        return;
    }
    if (err) {
        send_ota_error(c, err);
    } else {
        send_to(c, reply);
    }
}

// net_ota carries on an update over WiFi once the user answered the
// screen's question: yes erases and says ready, no or no answer fails it.
// Meanwhile the link counts as heard from, so it is neither dropped nor
// taken by another go-dock. Called by the net task, often.
static void net_ota(int64_t now)
{
    xSemaphoreTake(s_rx_lock, portMAX_DELAY);
    conn_t *c = s_ota_conn;
    if (c && c->kind == CONN_NET && c->fd >= 0 && ota_busy()) {
        int a = ota_asked();
        if (a > 0) {
            send_to(c, "{\"t\":\"ota\",\"state\":\"confirmed\"}");
            const char *err = ota_begin_asked(); // erases, about 10 s
            if (err) {
                send_ota_error(c, err);
            } else {
                send_to(c, "{\"t\":\"ota\",\"state\":\"ready\"}");
            }
            now = esp_timer_get_time();
        } else if (a < 0) {
            send_ota_error(c, a == -1 ? "已在屏幕上取消" : "屏幕上没有人确认，已取消");
        }
        if (ota_busy()) {
            c->last_rx_us = s_last_rx_us = now;
        }
    }
    xSemaphoreGive(s_rx_lock);
}

// wifi (§3.7, USB only): the network to join; the answer is a net message.
static void handle_wifi(conn_t *c, const cJSON *root)
{
    wlan_config_t cfg;
    copy_str(cfg.ssid, sizeof(cfg.ssid), cJSON_GetObjectItem(root, "ssid"));
    copy_str(cfg.pass, sizeof(cfg.pass), cJSON_GetObjectItem(root, "pass"));
    copy_str(cfg.ip, sizeof(cfg.ip), cJSON_GetObjectItem(root, "ip"));
    copy_str(cfg.mask, sizeof(cfg.mask), cJSON_GetObjectItem(root, "mask"));
    copy_str(cfg.gw, sizeof(cfg.gw), cJSON_GetObjectItem(root, "gw"));
    const char *err = wlan_configure(&cfg);
    memset(&cfg, 0, sizeof(cfg)); // the password stays in NVS only
    if (err) {
        send_log(c, "warn", err);
    }
    send_net(c);
}

// net_hello checks a TCP link's first hello: the pairing code, then
// whether the screen is free for it. Returns whether it now has the screen.
static bool net_hello(conn_t *c, const cJSON *root, int64_t now)
{
    const cJSON *code = cJSON_GetObjectItem(root, "code");
    if (!cJSON_IsString(code) || strcmp(code->valuestring, wlan_code()) != 0) {
        s_net_wait_us = now + NET_DENY_US; // slows down guessing
        s_net_wait_peer = c->peer;
        deny(c, "配对码不对，请对照屏幕上显示的配对码", now);
        return false;
    }
    c->authed = true;
    copy_str(c->name, sizeof(c->name), cJSON_GetObjectItem(root, "name"));
    copy_str(c->id, sizeof(c->id), cJSON_GetObjectItem(root, "id"));
    const conn_t *o = s_owner;
    bool same = o && c->id[0] && strcmp(o->id, c->id) == 0;
    if (o && o != c && !same && conn_open(o) && o->last_rx_us && now - o->last_rx_us < HOST_GONE_US) {
        char msg[120];
        snprintf(msg, sizeof(msg), "屏幕正被「%s」使用", o->name[0] ? o->name : "另一台电脑");
        deny(c, msg, now);
        return false;
    }
    ESP_LOGI(TAG, "%s has the screen over the network", c->name[0] ? c->name : "a go-dock");
    take_over(c, now);
    return true;
}

// handle_usb handles a message from the USB cable, which sets up the WiFi
// and updates the firmware; what the screen shows comes over WiFi. Updates
// work whatever the protocol version: they are how it gets fixed.
static void handle_usb(conn_t *c, const char *type, const cJSON *root)
{
    if (strcmp(type, "hello") == 0) {
        send_hello(c);
        send_net(c);
        ota_confirm(); // reached go-dock: a freshly updated image stays
    } else if (strncmp(type, "ota_", 4) == 0) {
        handle_ota(c, type, root);
    } else if (strcmp(type, "wifi") == 0) {
        handle_wifi(c, root);
    } else if (strcmp(type, "wifi_scan") == 0) {
        const char *err = wlan_scan();
        if (err) {
            send_scan(c, NULL, err);
        }
    }
}

// handle_line handles one message from c; the caller holds s_rx_lock.
static void handle_line(conn_t *c, char *line)
{
    if (c->kind == CONN_NET && c->close_us) {
        return; // turned away
    }
    cJSON *root = cJSON_Parse(line);
    if (!root) {
        return; // not a protocol message
    }
    const cJSON *t = cJSON_GetObjectItem(root, "t");
    if (!cJSON_IsString(t)) {
        cJSON_Delete(root);
        return;
    }
    const char *type = t->valuestring;
    int64_t now = esp_timer_get_time();
    if (c->kind == CONN_USB) {
        c->last_rx_us = now;
        handle_usb(c, type, root);
        cJSON_Delete(root);
        return;
    }
    if (!c->authed) {
        // Nothing counts before a hello with the pairing code.
        if (strcmp(type, "hello") != 0 || !net_hello(c, root, now)) {
            cJSON_Delete(root);
            return;
        }
    }
    if (c != s_owner) {
        cJSON_Delete(root);
        return;
    }
    c->last_rx_us = now;
    s_last_rx_us = now;

    if (strcmp(type, "hello") == 0) {
        int proto = (int)num(cJSON_GetObjectItem(root, "proto"));
        s_incompatible = proto != DOCK_PROTO;
        copy_str(c->name, sizeof(c->name), cJSON_GetObjectItem(root, "name"));
        copy_str(c->id, sizeof(c->id), cJSON_GetObjectItem(root, "id"));
        set_host_name(c->name);
        send_hello(c);
        ota_confirm(); // reached go-dock: a freshly updated image stays
        if (s_incompatible) {
            link_log("warn", "host speaks protocol %d, firmware speaks %d", proto, DOCK_PROTO);
        }
    } else if (strcmp(type, "snapshot") == 0) {
        ota_confirm();
        parse_snapshot(root, s_scratch);
        uint32_t seq = s_scratch->seq;
        xSemaphoreTake(s_lock, portMAX_DELAY);
        dock_snapshot_t *tmp = s_shared;
        s_shared = s_scratch;
        s_scratch = tmp;
        s_version++;
        s_received_us = now;
        xSemaphoreGive(s_lock);

        char ack[48];
        snprintf(ack, sizeof(ack), "{\"t\":\"ack\",\"seq\":%lu}", (unsigned long)seq);
        send_to(c, ack);
    } else if (strcmp(type, "line") == 0) {
        parse_line(root, s_line_scratch);
        xSemaphoreTake(s_lock, portMAX_DELAY);
        dock_line_t *tmp = s_line;
        s_line = s_line_scratch;
        s_line_scratch = tmp;
        s_line_version++;
        xSemaphoreGive(s_lock);
    } else if (strncmp(type, "ota_", 4) == 0) {
        handle_ota(c, type, root);
    } else if (strcmp(type, "settings") == 0 && !s_incompatible) {
        handle_settings(root);
    } else if (strcmp(type, "beep") == 0) {
        handle_beep(root);
    }
    cJSON_Delete(root);
}

// feed splits what a link read into lines and handles them.
static void feed(conn_t *c, const uint8_t *buf, int n)
{
    for (int i = 0; i < n; i++) {
        char ch = (char)buf[i];
        if (ch == '\n') {
            if (!c->skipping && c->len > 0) {
                if (c->line[c->len - 1] == '\r') {
                    c->len--;
                }
                c->line[c->len] = '\0';
                xSemaphoreTake(s_rx_lock, portMAX_DELAY);
                handle_line(c, c->line);
                xSemaphoreGive(s_rx_lock);
            }
            c->len = 0;
            c->skipping = false;
        } else if (!c->skipping) {
            if (c->len < MAX_LINE) {
                c->line[c->len++] = ch;
            } else {
                c->skipping = true;
                c->len = 0;
            }
        }
    }
}

static void usb_task(void *arg)
{
    static wlan_scan_t scan;
    uint8_t chunk[256];
    uint32_t net_version = wlan_version();
    uint32_t scan_version = 0;

    send_hello(&s_usb);
    for (;;) {
        int n = usb_serial_jtag_read_bytes(chunk, sizeof(chunk), pdMS_TO_TICKS(100));
        xSemaphoreTake(s_rx_lock, portMAX_DELAY); // the update may be the net task's
        ota_tick();
        xSemaphoreGive(s_rx_lock);
        if (n > 0) {
            feed(&s_usb, chunk, n);
        }
        // A go-dock on the cable hears how the WiFi is doing, even one that
        // took the hello sent at boot and never said its own.
        uint32_t v = wlan_version();
        if (v != net_version) {
            net_version = v;
            send_net(&s_usb);
        }
        // Scans are asked for over USB only, so their results go there.
        if (wlan_take_scan(&scan, &scan_version) && s_usb.last_rx_us) {
            send_scan(&s_usb, &scan, scan.err);
        }
    }
}

static int net_listen(void)
{
    int fd = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (fd < 0) {
        return -1;
    }
    int one = 1;
    setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
    struct sockaddr_in addr = {
        .sin_family = AF_INET,
        .sin_port = htons(WLAN_PORT),
        .sin_addr.s_addr = htonl(INADDR_ANY),
    };
    if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) != 0 || listen(fd, 2) != 0) {
        ESP_LOGE(TAG, "cannot listen on %d: errno %d", WLAN_PORT, errno);
        close(fd);
        return -1;
    }
    return fd;
}

static void net_refuse(int fd, const char *line)
{
    send(fd, line, strlen(line), 0);
    close(fd);
}

static void net_accept(int ls, int64_t now)
{
    struct sockaddr_in from;
    socklen_t len = sizeof(from);
    int fd = accept(ls, (struct sockaddr *)&from, &len);
    if (fd < 0) {
        return;
    }
    if (now < s_net_wait_us && from.sin_addr.s_addr == s_net_wait_peer) {
        net_refuse(fd, "{\"t\":\"denied\",\"msg\":\"刚有人填错了配对码，请稍后再试\"}\n");
        return;
    }
    conn_t *c = NULL;
    for (int i = 0; i < NET_CONNS && !c; i++) {
        if (s_net[i].fd < 0) {
            c = &s_net[i];
        }
    }
    if (!c) {
        net_refuse(fd, "{\"t\":\"denied\",\"msg\":\"连着屏幕的电脑太多了\"}\n");
        return;
    }
    // A host that stops reading must not hold up the screen for long.
    struct timeval tv = {.tv_sec = 1};
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));
    int one = 1;
    setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));

    xSemaphoreTake(s_rx_lock, portMAX_DELAY);
    xSemaphoreTake(s_tx_lock, portMAX_DELAY);
    c->fd = fd;
    c->broken = c->authed = false;
    c->opened_us = now;
    c->peer = from.sin_addr.s_addr;
    c->last_rx_us = c->close_us = 0;
    c->name[0] = c->id[0] = '\0';
    c->len = 0;
    c->skipping = false;
    xSemaphoreGive(s_tx_lock);
    xSemaphoreGive(s_rx_lock);
    char ip[16];
    inet_ntoa_r(from.sin_addr, ip, sizeof(ip));
    ESP_LOGI(TAG, "TCP link from %s", ip);
}

static void net_close(conn_t *c)
{
    xSemaphoreTake(s_rx_lock, portMAX_DELAY);
    xSemaphoreTake(s_tx_lock, portMAX_DELAY);
    if (c->fd >= 0) {
        close(c->fd);
    }
    c->fd = -1;
    if (s_owner == c) {
        s_owner = NULL; // the screen shows "主机离线" in a while
        s_incompatible = false; // that was about this go-dock
    }
    xSemaphoreGive(s_tx_lock);
    if (s_ota_conn == c) {
        if (ota_busy()) {
            ota_abort("与 go-dock 的连接断开了");
        }
        s_ota_conn = NULL;
    }
    xSemaphoreGive(s_rx_lock);
}

// net_task serves the TCP links (§1.1): accepts them, reads their lines and
// closes the ones that failed, were turned away or went quiet.
static void net_task(void *arg)
{
    uint8_t buf[512];
    int ls = -1;
    while (!wlan_ready()) {
        vTaskDelay(pdMS_TO_TICKS(200));
    }
    for (;;) {
        if (ls < 0 && (ls = net_listen()) < 0) {
            vTaskDelay(pdMS_TO_TICKS(2000));
            continue;
        }
        fd_set rd;
        FD_ZERO(&rd);
        FD_SET(ls, &rd);
        int maxfd = ls;
        for (int i = 0; i < NET_CONNS; i++) {
            if (s_net[i].fd >= 0) {
                FD_SET(s_net[i].fd, &rd);
                maxfd = s_net[i].fd > maxfd ? s_net[i].fd : maxfd;
            }
        }
        struct timeval tv = {.tv_sec = 0, .tv_usec = 200 * 1000};
        int n = select(maxfd + 1, &rd, NULL, NULL, &tv);
        if (n < 0) {
            ESP_LOGW(TAG, "select: errno %d", errno);
            vTaskDelay(pdMS_TO_TICKS(100));
            continue;
        }
        int64_t now = esp_timer_get_time();
        net_ota(now);
        if (n > 0 && FD_ISSET(ls, &rd)) {
            net_accept(ls, now);
        }
        for (int i = 0; i < NET_CONNS; i++) {
            conn_t *c = &s_net[i];
            if (c->fd < 0) {
                continue;
            }
            if (n > 0 && FD_ISSET(c->fd, &rd)) {
                int r = recv(c->fd, buf, sizeof(buf), 0);
                if (r <= 0) {
                    net_close(c);
                    continue;
                }
                feed(c, buf, r);
            }
            xSemaphoreTake(s_rx_lock, portMAX_DELAY);
            bool drop = c->broken || (c->close_us && now >= c->close_us) ||
                        (!c->authed && now - c->opened_us > NET_HELLO_US) ||
                        (c->authed && now - c->last_rx_us > NET_SILENT_US);
            xSemaphoreGive(s_rx_lock);
            if (drop) {
                net_close(c);
            }
        }
    }
}

void link_start(void)
{
    s_lock = xSemaphoreCreateMutex();
    s_tx_lock = xSemaphoreCreateMutex();
    s_rx_lock = xSemaphoreCreateMutex();
    s_shared = heap_caps_calloc(1, sizeof(dock_snapshot_t), MALLOC_CAP_SPIRAM);
    s_scratch = heap_caps_calloc(1, sizeof(dock_snapshot_t), MALLOC_CAP_SPIRAM);
    s_line = heap_caps_calloc(1, sizeof(dock_line_t), MALLOC_CAP_SPIRAM);
    s_line_scratch = heap_caps_calloc(1, sizeof(dock_line_t), MALLOC_CAP_SPIRAM);
    s_usb = (conn_t){.kind = CONN_USB, .fd = -1, .line = heap_caps_malloc(MAX_LINE + 1, MALLOC_CAP_SPIRAM)};
    for (int i = 0; i < NET_CONNS; i++) {
        s_net[i] = (conn_t){.kind = CONN_NET, .fd = -1, .line = heap_caps_malloc(MAX_LINE + 1, MALLOC_CAP_SPIRAM)};
    }

    // A snapshot and the line after it arrive back to back, up to 4 KB each.
    usb_serial_jtag_driver_config_t cfg = {
        .rx_buffer_size = 16384,
        .tx_buffer_size = 4096,
    };
    ESP_ERROR_CHECK(usb_serial_jtag_driver_install(&cfg));
    // Room for flash writes during an update.
    xTaskCreatePinnedToCore(usb_task, "link", 8192, NULL, 5, NULL, 0);
    xTaskCreatePinnedToCore(net_task, "net", 6144, NULL, 5, NULL, 0);
}

bool link_take_snapshot(dock_snapshot_t *out, uint32_t *version, int64_t *received_us)
{
    bool fresh = false;
    xSemaphoreTake(s_lock, portMAX_DELAY);
    if (s_version != *version) {
        memcpy(out, s_shared, sizeof(*out));
        *version = s_version;
        *received_us = s_received_us;
        fresh = true;
    }
    xSemaphoreGive(s_lock);
    return fresh;
}

bool link_take_line(dock_line_t *out, uint32_t *version)
{
    bool fresh = false;
    xSemaphoreTake(s_lock, portMAX_DELAY);
    if (s_line_version != *version) {
        memcpy(out, s_line, sizeof(*out));
        *version = s_line_version;
        fresh = true;
    }
    xSemaphoreGive(s_lock);
    return fresh;
}

void link_host_name(char *out, size_t n)
{
    xSemaphoreTake(s_lock, portMAX_DELAY);
    snprintf(out, n, "%s", s_host_name);
    xSemaphoreGive(s_lock);
}

int64_t link_last_rx_us(void)
{
    return s_last_rx_us;
}

bool link_incompatible(void)
{
    return s_incompatible;
}
