// go-dock firmware for the Waveshare ESP32-S3-Touch-AMOLED-2.41 (V2).
#include <limits.h>

#include "beep.h"
#include "display.h"
#include "esp_timer.h"
#include "ir.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "link.h"
#include "lvgl.h"
#include "ota.h"
#include "ui.h"

#define STACK_CHECK_US (10 * 1000000LL)

// A stack overflow restarts the screen without a word, so the stacks are
// watched: a task that came within warn bytes of its end tells go-dock once.
static const struct {
    const char *name;
    unsigned warn;
} STACKS[] = {{"lvgl", 2048}, {"link", 1024}, {"beep", 512}, {"ir_tx", 512}, {"ir_rx", 512}};

static void check_stacks(void)
{
    static bool warned[sizeof(STACKS) / sizeof(STACKS[0])];
    for (size_t i = 0; i < sizeof(STACKS) / sizeof(STACKS[0]); i++) {
        TaskHandle_t t = warned[i] ? NULL : xTaskGetHandle(STACKS[i].name);
        unsigned left = t ? (unsigned)uxTaskGetStackHighWaterMark(t) : UINT_MAX;
        if (left < STACKS[i].warn) {
            warned[i] = true;
            link_log("warn", "任务 %s 的栈最少只剩 %u 字节，再多用一点就会溢出重启", STACKS[i].name, left);
        }
    }
}

// All LVGL calls happen in this task.
static void lvgl_task(void *arg)
{
    display_init();
    ui_create();
    uint8_t brightness = 0;
    int64_t checked = 0;
    for (;;) {
        int64_t now = esp_timer_get_time();
        if (now - checked >= STACK_CHECK_US) {
            checked = now;
            check_stacks();
        }
        ui_poll();
        uint8_t want = ui_brightness();
        if (want != brightness) {
            display_set_brightness(want);
            brightness = want;
        }
        uint32_t wait = lv_timer_handler();
        if (wait < 2) {
            wait = 2;
        } else if (wait > 20) {
            wait = 20;
        }
        vTaskDelay(pdMS_TO_TICKS(wait));
    }
}

void app_main(void)
{
    ota_init();
    beep_init();
    ir_init();
    link_start();
    // LVGL draws nested objects recursively on this stack, with TinyTTF
    // rasterizing glyphs at the bottom: the lights page needs about 8.5 KB.
    xTaskCreatePinnedToCore(lvgl_task, "lvgl", 16384, NULL, 4, NULL, 1);
}
