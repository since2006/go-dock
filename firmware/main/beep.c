#include "beep.h"

#include <math.h>
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
// 13 bits is the finest the 80 MHz APB clock allows at 8000 Hz (a divider
// of 1.22); the quietest volumes need the small steps.
#define BEEP_RES LEDC_TIMER_13_BIT
#define BEEP_PERIOD (1 << 13) // duty counts in a period at BEEP_RES

typedef struct {
    int count;
    uint32_t duty;
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

// The duty for a volume in percent. What a passive buzzer plays is mostly
// the square wave's fundamental, whose amplitude goes with sin(pi * duty):
// loudest at half, and the same for duty and 1 - duty, so a narrower pulse
// is quieter whether the module switches on a high or a low level. Each
// percent below 100 takes 0.5 dB off, 45 dB at 10%; go-dock's browser
// preview (app.js playNotes) follows the same curve.
static uint32_t volume_duty(int volume)
{
    float amp = powf(10.0f, (volume - 100) * 0.5f / 20.0f);
    uint32_t duty = (uint32_t)(asinf(amp) / (float)M_PI * BEEP_PERIOD + 0.5f);
    return duty ? duty : 1;
}

static void tone(uint32_t hz, uint32_t duty)
{
    ledc_set_freq(BEEP_MODE, BEEP_TIMER, hz);
    ledc_set_duty(BEEP_MODE, BEEP_CHANNEL, duty);
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
                tone(n->hz, m.duty);
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
        .clk_cfg = LEDC_USE_APB_CLK, // see BEEP_RES
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

void beep_play(const beep_note_t *notes, int count, int volume)
{
    if (!s_queue) {
        return;
    }
    static melody_t m; // only the link task plays, and m is too big for its stack
    m.count = count < BEEP_MAX_NOTES ? count : BEEP_MAX_NOTES;
    m.duty = volume_duty(volume < 1 ? 1 : volume > 100 ? 100 : volume);
    memcpy(m.notes, notes, m.count * sizeof(*notes));
    xQueueOverwrite(s_queue, &m);
}
