#include "wlan.h"

#include <stdio.h>
#include <string.h>

#include "bootloader_random.h"
#include <stdlib.h>

#include "esp_event.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_netif.h"
#include "esp_random.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "nvs.h"

#define RETRY_MIN_US (1 * 1000000LL)
#define RETRY_MAX_US (30 * 1000000LL)

static const char *TAG = "wlan";
static const char *NS = "wifi";

static portMUX_TYPE s_mux = portMUX_INITIALIZER_UNLOCKED; // guards s_status, s_scan and their versions
static wlan_status_t s_status;
static uint32_t s_version;
static wlan_scan_t s_scan;
static uint32_t s_scan_version;
static volatile bool s_scanning;

// Changed only by wlan_configure, while WiFi is stopped.
static wlan_config_t s_cfg;
static char s_code[7];
static esp_netif_t *s_netif;
static esp_timer_handle_t s_retry;
static bool s_ready;  // network stack up
static bool s_driver; // esp_wifi_init done
static volatile bool s_want;          // should be on the network
static volatile int64_t s_backoff_us; // before the next try to join

static void set_state(wlan_state_t state, const char *ip, const char *err)
{
    char ipbuf[sizeof(s_status.ip)], errbuf[sizeof(s_status.err)];
    snprintf(ipbuf, sizeof(ipbuf), "%s", ip ? ip : "");
    snprintf(errbuf, sizeof(errbuf), "%s", err ? err : "");
    taskENTER_CRITICAL(&s_mux);
    s_status.state = state;
    memcpy(s_status.ip, ipbuf, sizeof(ipbuf));
    memcpy(s_status.err, errbuf, sizeof(errbuf));
    s_version++;
    taskEXIT_CRITICAL(&s_mux);
}

// set_network sets the network the status names, with state.
static void set_network(wlan_state_t state, const char *ssid, bool fixed)
{
    char name[sizeof(s_status.ssid)];
    snprintf(name, sizeof(name), "%s", ssid);
    taskENTER_CRITICAL(&s_mux);
    memcpy(s_status.ssid, name, sizeof(name));
    s_status.fixed = fixed;
    s_status.state = state;
    s_status.ip[0] = '\0';
    s_status.err[0] = '\0';
    s_version++;
    taskEXIT_CRITICAL(&s_mux);
}

wlan_status_t wlan_status(void)
{
    taskENTER_CRITICAL(&s_mux);
    wlan_status_t s = s_status;
    taskEXIT_CRITICAL(&s_mux);
    return s;
}

uint32_t wlan_version(void)
{
    taskENTER_CRITICAL(&s_mux);
    uint32_t v = s_version;
    taskEXIT_CRITICAL(&s_mux);
    return v;
}

const char *wlan_code(void)
{
    return s_code;
}

bool wlan_ready(void)
{
    return s_ready;
}

static const char *reason_text(uint8_t reason, char *buf, size_t n)
{
    switch (reason) {
    case WIFI_REASON_NO_AP_FOUND:
    case WIFI_REASON_NO_AP_FOUND_IN_RSSI_THRESHOLD:
        return "找不到这个 WiFi（只支持 2.4 GHz）";
    case WIFI_REASON_NO_AP_FOUND_W_COMPATIBLE_SECURITY:
    case WIFI_REASON_NO_AP_FOUND_IN_AUTHMODE_THRESHOLD:
        return "不支持这个 WiFi 的加密方式";
    case WIFI_REASON_AUTH_FAIL:
    case WIFI_REASON_4WAY_HANDSHAKE_TIMEOUT:
    case WIFI_REASON_HANDSHAKE_TIMEOUT:
        return "密码不对";
    case WIFI_REASON_ASSOC_FAIL:
    case WIFI_REASON_CONNECTION_FAIL:
        return "路由器拒绝连接";
    case WIFI_REASON_BEACON_TIMEOUT:
        return "WiFi 信号丢失";
    default:
        snprintf(buf, n, "连接断开（原因 %u）", reason);
        return buf;
    }
}

static void scan_done(const wifi_event_sta_scan_done_t *d);

static void on_event(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    (void)arg;
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        if (s_want) {
            esp_wifi_connect();
        }
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        const wifi_event_sta_disconnected_t *d = data;
        if (!s_want || d->reason == WIFI_REASON_ASSOC_LEAVE) {
            return; // we left: wlan_configure is at work
        }
        char buf[40];
        if (wlan_status().state == WLAN_CONNECTED) {
            // Lost a network it was on (router restarting): not a failure yet.
            s_backoff_us = RETRY_MIN_US;
            set_state(WLAN_CONNECTING, NULL, NULL);
        } else {
            set_state(WLAN_FAILED, NULL, reason_text(d->reason, buf, sizeof(buf)));
        }
        ESP_LOGI(TAG, "disconnected (reason %u), next try in %lld s", d->reason, s_backoff_us / 1000000);
        esp_timer_stop(s_retry);
        esp_timer_start_once(s_retry, s_backoff_us);
        int64_t next = s_backoff_us * 2;
        s_backoff_us = next > RETRY_MAX_US ? RETRY_MAX_US : next;
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_SCAN_DONE) {
        if (s_scanning) {
            scan_done(data); // ours, not one the driver ran to join
        }
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        const ip_event_got_ip_t *e = data;
        char ip[16];
        snprintf(ip, sizeof(ip), IPSTR, IP2STR(&e->ip_info.ip));
        s_backoff_us = RETRY_MIN_US;
        set_state(WLAN_CONNECTED, ip, NULL);
        ESP_LOGI(TAG, "connected as %s", ip);
    }
}

static void on_retry(void *arg)
{
    (void)arg;
    if (s_want && !s_scanning) {
        esp_wifi_connect();
    }
}

static bool driver_up(void)
{
    if (s_driver) {
        return true;
    }
    wifi_init_config_t init = WIFI_INIT_CONFIG_DEFAULT();
    esp_err_t err = esp_wifi_init(&init);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_wifi_init: %s", esp_err_to_name(err));
        return false;
    }
    esp_wifi_set_storage(WIFI_STORAGE_RAM); // the settings live in our namespace
    esp_wifi_set_mode(WIFI_MODE_STA);
    s_driver = true;
    return true;
}

static int by_rssi(const void *a, const void *b)
{
    return ((const wlan_ap_t *)b)->rssi - ((const wlan_ap_t *)a)->rssi;
}

// after_scan puts the radio back as it was: off when no network is set,
// else trying to join again if it had not.
static void after_scan(void)
{
    s_scanning = false;
    if (!s_cfg.ssid[0]) {
        esp_wifi_stop();
    } else if (s_want && wlan_status().state != WLAN_CONNECTED) {
        esp_wifi_connect();
    }
}

// scan_done keeps the strongest access point of each named network.
static void scan_done(const wifi_event_sta_scan_done_t *d)
{
    static wlan_scan_t res; // the event task only
    memset(&res, 0, sizeof(res));
    uint16_t n = 0;
    esp_wifi_scan_get_ap_num(&n);
    wifi_ap_record_t *recs = n ? heap_caps_calloc(n, sizeof(*recs), MALLOC_CAP_SPIRAM) : NULL;
    wlan_ap_t *all = n ? heap_caps_calloc(n, sizeof(*all), MALLOC_CAP_SPIRAM) : NULL;
    int count = 0;
    if (d->status != 0) {
        esp_wifi_clear_ap_list();
        snprintf(res.err, sizeof(res.err), "扫描失败，请再试一次");
    } else if (n && (!recs || !all)) {
        esp_wifi_clear_ap_list();
        snprintf(res.err, sizeof(res.err), "内存不足，扫描失败");
    } else if (n) {
        esp_wifi_scan_get_ap_records(&n, recs); // also frees the driver's list
        for (int i = 0; i < n; i++) {
            const char *ssid = (const char *)recs[i].ssid;
            if (!ssid[0]) {
                continue; // hidden
            }
            int j = 0;
            while (j < count && strcmp(all[j].ssid, ssid) != 0) {
                j++;
            }
            if (j == count) {
                snprintf(all[count].ssid, sizeof(all[count].ssid), "%s", ssid);
                all[count].rssi = recs[i].rssi;
                all[count].open = recs[i].authmode == WIFI_AUTH_OPEN;
                count++;
            } else if (recs[i].rssi > all[j].rssi) {
                all[j].rssi = recs[i].rssi;
            }
        }
        qsort(all, count, sizeof(*all), by_rssi);
        res.count = count < WLAN_MAX_APS ? count : WLAN_MAX_APS;
        memcpy(res.aps, all, res.count * sizeof(*all));
    }
    heap_caps_free(recs);
    heap_caps_free(all);
    ESP_LOGI(TAG, "scan found %d networks", count);
    taskENTER_CRITICAL(&s_mux);
    s_scan = res;
    s_scan_version++;
    taskEXIT_CRITICAL(&s_mux);
    after_scan();
}

const char *wlan_scan(void)
{
    if (!s_ready) {
        return "WiFi 还没准备好，请稍后再试";
    }
    if (s_scanning) {
        return NULL; // the result of the one under way will do
    }
    if (!driver_up()) {
        return "WiFi 启动失败";
    }
    if (!s_cfg.ssid[0]) {
        // No network set: the radio is off, on just for the scan.
        if (esp_wifi_start() != ESP_OK) {
            return "WiFi 启动失败";
        }
    } else if (wlan_status().state != WLAN_CONNECTED) {
        // A scan cannot run while it tries to join: stop trying meanwhile.
        esp_timer_stop(s_retry);
        esp_wifi_disconnect();
    }
    s_scanning = true;
    wifi_scan_config_t sc = {.show_hidden = false, .scan_type = WIFI_SCAN_TYPE_ACTIVE};
    esp_err_t err = esp_wifi_scan_start(&sc, false);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "esp_wifi_scan_start: %s", esp_err_to_name(err));
        after_scan();
        return "现在不能扫描，请稍后再试";
    }
    return NULL;
}

bool wlan_take_scan(wlan_scan_t *out, uint32_t *version)
{
    bool fresh = false;
    taskENTER_CRITICAL(&s_mux);
    if (s_scan_version != *version) {
        *out = s_scan;
        *version = s_scan_version;
        fresh = true;
    }
    taskEXIT_CRITICAL(&s_mux);
    return fresh;
}

static void load_str(nvs_handle_t h, const char *key, char *dst, size_t n)
{
    size_t len = n;
    if (nvs_get_str(h, key, dst, &len) != ESP_OK) {
        dst[0] = '\0';
    }
}

static bool valid_code(const char *c)
{
    for (int i = 0; i < 6; i++) {
        if (c[i] < '0' || c[i] > '9') {
            return false;
        }
    }
    return c[6] == '\0';
}

static void save_config(void)
{
    nvs_handle_t h;
    esp_err_t err = nvs_open(NS, NVS_READWRITE, &h);
    if (err == ESP_OK) {
        nvs_set_str(h, "ssid", s_cfg.ssid);
        nvs_set_str(h, "pass", s_cfg.pass);
        nvs_set_str(h, "ip", s_cfg.ip);
        nvs_set_str(h, "mask", s_cfg.mask);
        nvs_set_str(h, "gw", s_cfg.gw);
        nvs_set_str(h, "code", s_code);
        err = nvs_commit(h);
        nvs_close(h);
    }
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "saving failed: %s", esp_err_to_name(err));
    }
}

// load_config reads the saved network, and makes up the pairing code the
// first time. The radio is off yet, so the RNG takes its entropy from the
// source the bootloader uses.
static void load_config(void)
{
    nvs_handle_t h;
    if (nvs_open(NS, NVS_READONLY, &h) == ESP_OK) {
        load_str(h, "ssid", s_cfg.ssid, sizeof(s_cfg.ssid));
        load_str(h, "pass", s_cfg.pass, sizeof(s_cfg.pass));
        load_str(h, "ip", s_cfg.ip, sizeof(s_cfg.ip));
        load_str(h, "mask", s_cfg.mask, sizeof(s_cfg.mask));
        load_str(h, "gw", s_cfg.gw, sizeof(s_cfg.gw));
        load_str(h, "code", s_code, sizeof(s_code));
        nvs_close(h);
    }
    if (!valid_code(s_code)) {
        bootloader_random_enable();
        uint32_t r = esp_random() % 1000000;
        bootloader_random_disable();
        snprintf(s_code, sizeof(s_code), "%06lu", (unsigned long)r);
        save_config();
    }
}

// join starts WiFi on s_cfg, which names a network; WiFi is stopped.
static const char *join(void)
{
    if (!driver_up()) {
        set_state(WLAN_FAILED, NULL, "WiFi 启动失败");
        return "WiFi 启动失败";
    }
    wifi_config_t wc = {0};
    memcpy(wc.sta.ssid, s_cfg.ssid, strnlen(s_cfg.ssid, sizeof(wc.sta.ssid)));
    memcpy(wc.sta.password, s_cfg.pass, strnlen(s_cfg.pass, sizeof(wc.sta.password)));
    wc.sta.threshold.authmode = WIFI_AUTH_OPEN; // whatever the network uses
    wc.sta.scan_method = WIFI_ALL_CHANNEL_SCAN; // the strongest of several access points
    wc.sta.sort_method = WIFI_CONNECT_AP_BY_SIGNAL;
    esp_wifi_set_config(WIFI_IF_STA, &wc);

    if (s_cfg.ip[0]) {
        esp_netif_dhcpc_stop(s_netif);
        esp_netif_ip_info_t info = {0};
        esp_netif_str_to_ip4(s_cfg.ip, &info.ip);
        esp_netif_str_to_ip4(s_cfg.mask, &info.netmask);
        if (s_cfg.gw[0]) {
            esp_netif_str_to_ip4(s_cfg.gw, &info.gw);
        }
        esp_netif_set_ip_info(s_netif, &info);
    } else {
        esp_netif_dhcpc_start(s_netif); // starts once connected
    }

    set_network(WLAN_CONNECTING, s_cfg.ssid, s_cfg.ip[0] != '\0');
    s_backoff_us = RETRY_MIN_US;
    s_want = true;
    esp_err_t err = esp_wifi_start();
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_wifi_start: %s", esp_err_to_name(err));
        set_state(WLAN_FAILED, NULL, "WiFi 启动失败");
        return "WiFi 启动失败";
    }
    // The screen is lit anyway; power saving would delay what go-dock sends
    // by up to a few hundred milliseconds.
    esp_wifi_set_ps(WIFI_PS_NONE);
    ESP_LOGI(TAG, "joining %s (%s)", s_cfg.ssid, s_cfg.ip[0] ? s_cfg.ip : "DHCP");
    return NULL;
}

void wlan_start(void)
{
    load_config();
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    s_netif = esp_netif_create_default_wifi_sta();
    esp_netif_set_hostname(s_netif, "go-dock");
    ESP_ERROR_CHECK(esp_event_handler_instance_register(WIFI_EVENT, ESP_EVENT_ANY_ID, on_event, NULL, NULL));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(IP_EVENT, IP_EVENT_STA_GOT_IP, on_event, NULL, NULL));
    const esp_timer_create_args_t retry = {.callback = on_retry, .name = "wlan_retry"};
    ESP_ERROR_CHECK(esp_timer_create(&retry, &s_retry));
    if (s_cfg.ssid[0]) {
        join();
    }
    // Ready once the first join is under way: before, a wifi or wifi_scan
    // from USB would touch the driver while join sets it up. The pairing
    // code is known now: the link reports it to go-dock.
    s_ready = true;
    taskENTER_CRITICAL(&s_mux);
    s_version++;
    taskEXIT_CRITICAL(&s_mux);
}

const char *wlan_configure(const wlan_config_t *c)
{
    if (!s_ready) {
        return "WiFi 还没准备好，请稍后再试";
    }
    if (s_scanning) {
        return "正在扫描 WiFi，请几秒后再试";
    }
    wlan_config_t next = *c;
    next.ssid[sizeof(next.ssid) - 1] = next.pass[sizeof(next.pass) - 1] = '\0';
    next.ip[sizeof(next.ip) - 1] = next.mask[sizeof(next.mask) - 1] = next.gw[sizeof(next.gw) - 1] = '\0';
    if (!next.ssid[0]) {
        memset(&next, 0, sizeof(next));
    } else if (next.pass[0] && strlen(next.pass) < 8) {
        return "WiFi 密码至少 8 个字符";
    } else if (!next.ip[0]) {
        next.mask[0] = next.gw[0] = '\0';
    } else {
        if (!next.mask[0]) {
            snprintf(next.mask, sizeof(next.mask), "255.255.255.0");
        }
        esp_ip4_addr_t a;
        if (esp_netif_str_to_ip4(next.ip, &a) != ESP_OK || a.addr == 0) {
            return "固定 IP 不对";
        }
        if (esp_netif_str_to_ip4(next.mask, &a) != ESP_OK || a.addr == 0) {
            return "子网掩码不对";
        }
        if (next.gw[0] && esp_netif_str_to_ip4(next.gw, &a) != ESP_OK) {
            return "网关不对";
        }
    }

    s_want = false;
    esp_timer_stop(s_retry);
    if (s_driver) {
        esp_wifi_stop();
    }
    s_cfg = next;
    save_config();
    if (!s_cfg.ssid[0]) {
        set_network(WLAN_OFF, "", false);
        ESP_LOGI(TAG, "off");
        return NULL;
    }
    return join();
}
