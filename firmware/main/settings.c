#include "settings.h"

#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "nvs.h"
#include "nvs_flash.h"

static const char *TAG = "settings";
static const char *NS = "screen";

static portMUX_TYPE s_mux = portMUX_INITIALIZER_UNLOCKED;
static settings_t s_cur = {.brightness = 80, .dim_brightness = 20, .dim_after = 300};

void settings_init(void)
{
    esp_err_t err = nvs_flash_init();
    if (err == ESP_ERR_NVS_NO_FREE_PAGES || err == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        nvs_flash_erase(); // written by something else: start over
        err = nvs_flash_init();
    }
    nvs_handle_t h;
    if (err != ESP_OK || nvs_open(NS, NVS_READONLY, &h) != ESP_OK) {
        return; // nothing saved yet
    }
    uint8_t u8;
    uint32_t u32;
    if (nvs_get_u8(h, "bright", &u8) == ESP_OK && u8 >= 10 && u8 <= 100) {
        s_cur.brightness = u8;
    }
    if (nvs_get_u8(h, "dim", &u8) == ESP_OK && u8 <= 100) {
        s_cur.dim_brightness = u8;
    }
    if (nvs_get_u32(h, "dim_after", &u32) == ESP_OK && u32 <= 24 * 3600) {
        s_cur.dim_after = u32;
    }
    nvs_close(h);
}

settings_t settings_get(void)
{
    taskENTER_CRITICAL(&s_mux);
    settings_t s = s_cur;
    taskEXIT_CRITICAL(&s_mux);
    return s;
}

bool settings_set(settings_t s)
{
    taskENTER_CRITICAL(&s_mux);
    settings_t old = s_cur;
    s_cur = s;
    taskEXIT_CRITICAL(&s_mux);
    if (s.brightness == old.brightness && s.dim_brightness == old.dim_brightness && s.dim_after == old.dim_after) {
        return false; // go-dock sends them on every connect
    }
    nvs_handle_t h;
    esp_err_t err = nvs_open(NS, NVS_READWRITE, &h);
    if (err == ESP_OK) {
        nvs_set_u8(h, "bright", s.brightness);
        nvs_set_u8(h, "dim", s.dim_brightness);
        nvs_set_u32(h, "dim_after", s.dim_after);
        err = nvs_commit(h);
        nvs_close(h);
    }
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "saving failed: %s", esp_err_to_name(err));
    }
    return true;
}
