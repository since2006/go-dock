#include "beep.h"

#include <string.h>

#include "driver/gpio.h"
#include "driver/ledc.h"
#include "esp_rom_gpio.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/task.h"
#include "hal/ledc_periph.h"

#define PIN_BUZZER 1 // J8 pin 3; pin 1 next to it is GND
#define BEEP_MODE LEDC_LOW_SPEED_MODE
#define BEEP_TIMER LEDC_TIMER_0
#define BEEP_CHANNEL LEDC_CHANNEL_0
#define BEEP_RES LEDC_TIMER_10_BIT
#define BEEP_DUTY (1 << 9) // a square wave: the loudest a passive buzzer gets

typedef struct {
    int count;
    beep_note_t notes[BEEP_MAX_NOTES];
} melody_t;

static QueueHandle_t s_queue; // holds the next melody, newest wins

// Between notes the pin floats, so a buzzer module draws no current
// whether it switches on a high or a low level.
static void silence(void)
{
    ledc_stop(BEEP_MODE, BEEP_CHANNEL, 0);
    gpio_set_direction(PIN_BUZZER, GPIO_MODE_DISABLE);
}

static void tone(uint32_t hz)
{
    ledc_set_freq(BEEP_MODE, BEEP_TIMER, hz);
    ledc_set_duty(BEEP_MODE, BEEP_CHANNEL, BEEP_DUTY);
    ledc_update_duty(BEEP_MODE, BEEP_CHANNEL);
    // silence() took the pin off the channel; this is the routing
    // ledc_channel_config does, without reserving the pin again.
    esp_rom_gpio_connect_out_signal(PIN_BUZZER, ledc_periph_signal[0].speed_mode[BEEP_MODE].sig_out_idx[BEEP_CHANNEL],
                                    false, false);
}

static void beep_task(void *arg)
{
    static melody_t m; // too big for the task's stack
    int next = 0;
    for (;;) {
        TickType_t wait = portMAX_DELAY;
        if (next < m.count) {
            const beep_note_t *n = &m.notes[next++];
            if (n->hz) {
                tone(n->hz);
            } else {
                silence();
            }
            wait = pdMS_TO_TICKS(n->ms);
        } else {
            silence();
        }
        // A new melody cuts off the one playing.
        if (xQueueReceive(s_queue, &m, wait) == pdTRUE) {
            next = 0;
        }
    }
}

void beep_init(void)
{
    ledc_timer_config_t timer = {
        .speed_mode = BEEP_MODE,
        .duty_resolution = BEEP_RES,
        .timer_num = BEEP_TIMER,
        .freq_hz = 2000,
        .clk_cfg = LEDC_AUTO_CLK,
    };
    ESP_ERROR_CHECK(ledc_timer_config(&timer));
    ledc_channel_config_t ch = {
        .gpio_num = PIN_BUZZER,
        .speed_mode = BEEP_MODE,
        .channel = BEEP_CHANNEL,
        .timer_sel = BEEP_TIMER,
        .duty = 0,
    };
    ESP_ERROR_CHECK(ledc_channel_config(&ch));
    gpio_set_pull_mode(PIN_BUZZER, GPIO_FLOATING);
    silence();

    s_queue = xQueueCreate(1, sizeof(melody_t));
    xTaskCreate(beep_task, "beep", 2048, NULL, 3, NULL);
}

void beep_play(const beep_note_t *notes, int count)
{
    if (!s_queue) {
        return;
    }
    static melody_t m; // only the link task plays, and m is too big for its stack
    m.count = count < BEEP_MAX_NOTES ? count : BEEP_MAX_NOTES;
    memcpy(m.notes, notes, m.count * sizeof(*notes));
    xQueueOverwrite(s_queue, &m);
}
