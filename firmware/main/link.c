#include "link.h"

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
#include "ota.h"

#define MAX_LINE 4096

static const char *TAG = "link";

static SemaphoreHandle_t s_lock;     // guards s_shared, s_version, s_received_us and the pages
static SemaphoreHandle_t s_tx_lock;  // keeps lines from different tasks whole
static dock_snapshot_t *s_shared;    // newest complete snapshot
static dock_snapshot_t *s_scratch;   // parse target, owned by the reader task
static uint32_t s_version;
static dock_pages_t *s_pages;         // newest function pages
static dock_pages_t *s_pages_scratch; // parse target, owned by the reader task
static uint32_t s_pages_version;
static int64_t s_received_us;
static volatile int64_t s_last_rx_us;
static volatile bool s_incompatible;
static bool s_ota; // can be updated over the link

static void send_line(const char *json)
{
    xSemaphoreTake(s_tx_lock, portMAX_DELAY);
    // Short timeouts: with no host attached the write would only block.
    usb_serial_jtag_write_bytes(json, strlen(json), pdMS_TO_TICKS(50));
    usb_serial_jtag_write_bytes("\n", 1, pdMS_TO_TICKS(50));
    xSemaphoreGive(s_tx_lock);
}

static void send_hello(void)
{
    char buf[176];
    snprintf(buf, sizeof(buf),
             "{\"t\":\"hello\",\"proto\":%d,\"fw\":\"%s\",\"w\":600,\"h\":450,\"max_line\":%d%s,\"beep\":1,"
             "\"pages\":1}",
             DOCK_PROTO, esp_app_get_description()->version, MAX_LINE, s_ota ? ",\"ota\":1" : "");
    send_line(buf);
}

void link_log(const char *level, const char *fmt, ...)
{
    char msg[200];
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(msg, sizeof(msg), fmt, ap);
    va_end(ap);
    ESP_LOGI(TAG, "%s", msg);

    cJSON *o = cJSON_CreateObject();
    cJSON_AddStringToObject(o, "t", "log");
    cJSON_AddStringToObject(o, "level", level);
    cJSON_AddStringToObject(o, "msg", msg);
    char *line = cJSON_PrintUnformatted(o);
    if (line) {
        send_line(line);
        cJSON_free(line);
    }
    cJSON_Delete(o);
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
        copy_str(d->label, sizeof(d->label), cJSON_GetObjectItem(item, "label"));
        copy_str(d->text, sizeof(d->text), cJSON_GetObjectItem(item, "text"));
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

// Copies an id the device echoes back in an action; only [a-z0-9_-]{1,16}
// passes, so it never needs escaping.
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

// pages: the function pages, replacing the ones before. Pages and items
// with ids the device could not echo back are left out.
static void parse_pages(const cJSON *root, dock_pages_t *out)
{
    memset(out, 0, sizeof(*out));
    const cJSON *pg;
    cJSON_ArrayForEach(pg, cJSON_GetObjectItem(root, "pages"))
    {
        if (out->count >= DOCK_MAX_PAGES) {
            break;
        }
        dock_page_t *p = &out->pages[out->count];
        if (!copy_id(p->id, cJSON_GetObjectItem(pg, "id"))) {
            continue;
        }
        out->count++;
        copy_str(p->title, sizeof(p->title), cJSON_GetObjectItem(pg, "title"));
        copy_str(p->summary, sizeof(p->summary), cJSON_GetObjectItem(pg, "summary"));
        copy_str(p->error, sizeof(p->error), cJSON_GetObjectItem(pg, "error"));
        const cJSON *it;
        cJSON_ArrayForEach(it, cJSON_GetObjectItem(pg, "items"))
        {
            if (p->item_count >= DOCK_MAX_ITEMS) {
                break;
            }
            dock_item_t *d = &p->items[p->item_count];
            if (!copy_id(d->id, cJSON_GetObjectItem(it, "id"))) {
                continue;
            }
            p->item_count++;
            const cJSON *kind = cJSON_GetObjectItem(it, "kind");
            d->kind = cJSON_IsString(kind) && strcmp(kind->valuestring, "switch") == 0 ? DOCK_ITEM_SWITCH : DOCK_ITEM_OTHER;
            copy_str(d->label, sizeof(d->label), cJSON_GetObjectItem(it, "label"));
            copy_str(d->text, sizeof(d->text), cJSON_GetObjectItem(it, "text"));
            const cJSON *on = cJSON_GetObjectItem(it, "on");
            d->on = cJSON_IsBool(on) ? (cJSON_IsTrue(on) ? 1 : 0) : -1;
            d->busy = cJSON_IsTrue(cJSON_GetObjectItem(it, "busy"));
            d->disabled = cJSON_IsTrue(cJSON_GetObjectItem(it, "disabled"));
        }
    }
}

// beep: {"notes":[[hz,ms],...]}; a frequency out of range plays as a rest.
static void handle_beep(const cJSON *root)
{
    beep_note_t notes[BEEP_MAX_NOTES];
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
    beep_play(notes, n);
}

static void send_ota_error(const char *err)
{
    cJSON *o = cJSON_CreateObject();
    cJSON_AddStringToObject(o, "t", "ota");
    cJSON_AddStringToObject(o, "state", "error");
    cJSON_AddStringToObject(o, "msg", err);
    char *line = cJSON_PrintUnformatted(o);
    if (line) {
        send_line(line);
        cJSON_free(line);
    }
    cJSON_Delete(o);
}

// Firmware update messages work whatever the protocol version: they are
// how a device too old for its host gets fixed.
static void handle_ota(const char *t, const cJSON *root)
{
    char reply[64];
    const char *err;
    if (strcmp(t, "ota_begin") == 0) {
        const cJSON *ver = cJSON_GetObjectItem(root, "ver");
        err = ota_begin((size_t)num(cJSON_GetObjectItem(root, "size")), cJSON_IsString(ver) ? ver->valuestring : "");
        snprintf(reply, sizeof(reply), "{\"t\":\"ota\",\"state\":\"ready\"}");
    } else if (strcmp(t, "ota_data") == 0) {
        const cJSON *data = cJSON_GetObjectItem(root, "data");
        size_t written = 0;
        err = ota_write((size_t)num(cJSON_GetObjectItem(root, "off")), cJSON_IsString(data) ? data->valuestring : "",
                        &written);
        snprintf(reply, sizeof(reply), "{\"t\":\"ota\",\"state\":\"ack\",\"off\":%u}", (unsigned)written);
    } else if (strcmp(t, "ota_end") == 0) {
        err = ota_finish();
        if (!err) {
            send_line("{\"t\":\"ota\",\"state\":\"done\"}");
            usb_serial_jtag_wait_tx_done(pdMS_TO_TICKS(200));
            vTaskDelay(pdMS_TO_TICKS(1000)); // the screen shows the restart
            esp_restart();
        }
    } else if (strcmp(t, "ota_abort") == 0) {
        ota_abort();
        return;
    } else {
        return;
    }
    if (err) {
        send_ota_error(err);
    } else {
        send_line(reply);
    }
}

static void handle_line(char *line)
{
    cJSON *root = cJSON_Parse(line);
    if (!root) {
        return; // not a protocol message
    }
    const cJSON *t = cJSON_GetObjectItem(root, "t");
    if (!cJSON_IsString(t)) {
        cJSON_Delete(root);
        return;
    }
    s_last_rx_us = esp_timer_get_time();

    if (strcmp(t->valuestring, "hello") == 0) {
        int proto = (int)num(cJSON_GetObjectItem(root, "proto"));
        s_incompatible = proto != DOCK_PROTO;
        send_hello();
        ota_confirm(); // reached go-dock: a freshly updated image stays
        if (s_incompatible) {
            link_log("warn", "host speaks protocol %d, firmware speaks %d", proto, DOCK_PROTO);
        }
    } else if (strncmp(t->valuestring, "ota_", 4) == 0) {
        handle_ota(t->valuestring, root);
    } else if (strcmp(t->valuestring, "snapshot") == 0) {
        ota_confirm();
        parse_snapshot(root, s_scratch);
        uint32_t seq = s_scratch->seq;
        xSemaphoreTake(s_lock, portMAX_DELAY);
        dock_snapshot_t *tmp = s_shared;
        s_shared = s_scratch;
        s_scratch = tmp;
        s_version++;
        s_received_us = s_last_rx_us;
        xSemaphoreGive(s_lock);

        char ack[48];
        snprintf(ack, sizeof(ack), "{\"t\":\"ack\",\"seq\":%lu}", (unsigned long)seq);
        send_line(ack);
    } else if (strcmp(t->valuestring, "pages") == 0) {
        parse_pages(root, s_pages_scratch);
        xSemaphoreTake(s_lock, portMAX_DELAY);
        dock_pages_t *tmp = s_pages;
        s_pages = s_pages_scratch;
        s_pages_scratch = tmp;
        s_pages_version++;
        xSemaphoreGive(s_lock);
    } else if (strcmp(t->valuestring, "beep") == 0) {
        handle_beep(root);
    }
    cJSON_Delete(root);
}

static void reader_task(void *arg)
{
    char *line = heap_caps_malloc(MAX_LINE + 1, MALLOC_CAP_SPIRAM);
    uint8_t chunk[256];
    size_t len = 0;
    bool skipping = false; // inside a line longer than MAX_LINE

    send_hello();
    for (;;) {
        int n = usb_serial_jtag_read_bytes(chunk, sizeof(chunk), pdMS_TO_TICKS(100));
        ota_tick();
        for (int i = 0; i < n; i++) {
            char c = (char)chunk[i];
            if (c == '\n') {
                if (!skipping && len > 0) {
                    if (line[len - 1] == '\r') {
                        len--;
                    }
                    line[len] = '\0';
                    handle_line(line);
                }
                len = 0;
                skipping = false;
            } else if (!skipping) {
                if (len < MAX_LINE) {
                    line[len++] = c;
                } else {
                    skipping = true;
                    len = 0;
                }
            }
        }
    }
}

void link_start(void)
{
    s_lock = xSemaphoreCreateMutex();
    s_tx_lock = xSemaphoreCreateMutex();
    s_ota = ota_supported();
    s_shared = heap_caps_calloc(1, sizeof(dock_snapshot_t), MALLOC_CAP_SPIRAM);
    s_scratch = heap_caps_calloc(1, sizeof(dock_snapshot_t), MALLOC_CAP_SPIRAM);
    s_pages = heap_caps_calloc(1, sizeof(dock_pages_t), MALLOC_CAP_SPIRAM);
    s_pages_scratch = heap_caps_calloc(1, sizeof(dock_pages_t), MALLOC_CAP_SPIRAM);

    // A snapshot and the pages after it arrive back to back, up to 4 KB each.
    usb_serial_jtag_driver_config_t cfg = {
        .rx_buffer_size = 16384,
        .tx_buffer_size = 4096,
    };
    ESP_ERROR_CHECK(usb_serial_jtag_driver_install(&cfg));
    // Room for flash writes during an update.
    xTaskCreatePinnedToCore(reader_task, "link", 8192, NULL, 5, NULL, 0);
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

bool link_take_pages(dock_pages_t *out, uint32_t *version)
{
    bool fresh = false;
    xSemaphoreTake(s_lock, portMAX_DELAY);
    if (s_pages_version != *version) {
        memcpy(out, s_pages, sizeof(*out));
        *version = s_pages_version;
        fresh = true;
    }
    xSemaphoreGive(s_lock);
    return fresh;
}

void link_send_action(const char *page, const char *item, const char *act)
{
    char buf[112];
    snprintf(buf, sizeof(buf), "{\"t\":\"action\",\"page\":\"%s\",\"item\":\"%s\",\"act\":\"%s\"}", page, item, act);
    send_line(buf);
}

int64_t link_last_rx_us(void)
{
    return s_last_rx_us;
}

bool link_incompatible(void)
{
    return s_incompatible;
}
