// go-dock firmware for the Waveshare ESP32-S3-Touch-AMOLED-2.41 (V2).
#include "beep.h"
#include "display.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "link.h"
#include "lvgl.h"
#include "ota.h"
#include "ui.h"

// All LVGL calls happen in this task.
static void lvgl_task(void *arg)
{
    display_init();
    ui_create();
    uint8_t brightness = 0;
    for (;;) {
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
    link_start();
    xTaskCreatePinnedToCore(lvgl_task, "lvgl", 8192, NULL, 4, NULL, 1);
}
