#include "ir.h"

#include <string.h>

#include "driver/gpio.h"
#include "driver/rmt_rx.h"
#include "driver/rmt_tx.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/task.h"
#include "link.h"

#define PIN_IR_TX 7       // J8 pin 13
#define PIN_IR_RX 8       // J8 pin 15
#define IR_TX_INVERT 0    // 1 for a transmitter module that lights on a low level
#define RESOLUTION 1000000 // 1 µs per tick
#define RX_SYMBOLS 192     // all four RX memory blocks: 384 timings
#define RX_IDLE_NS (30 * 1000 * 1000) // a frame ends after 30 ms quiet (Gree has a 20 ms gap inside)
#define RX_GLITCH_NS 1250
#define ECHO_US (50 * 1000) // what the receiver hears this soon after sending is our own

static const char *TAG = "ir";

typedef struct {
    uint32_t hz;
    int count;
    uint16_t raw[IR_MAX_RAW];
} frame_t;

static rmt_channel_handle_t s_tx, s_rx;
static rmt_encoder_handle_t s_copy;
static QueueHandle_t s_tx_queue; // frames to send
static QueueHandle_t s_rx_queue; // symbol counts of received frames
static rmt_symbol_word_t s_tx_symbols[IR_MAX_RAW / 2 + 1];
static rmt_symbol_word_t s_rx_symbols[RX_SYMBOLS];

static portMUX_TYPE s_lock = portMUX_INITIALIZER_UNLOCKED; // guards the three below
static bool s_sending;
static int64_t s_tx_start_us; // when the latest frame started going out
static int64_t s_quiet_us;    // until when the receiver may still hear it

static void sending(bool on)
{
    int64_t now = esp_timer_get_time();
    portENTER_CRITICAL(&s_lock);
    s_sending = on;
    if (on) {
        s_tx_start_us = now;
    } else {
        s_quiet_us = now + ECHO_US;
    }
    portEXIT_CRITICAL(&s_lock);
}

// Whether a frame heard from start to end overlaps one of ours.
static bool echo(int64_t start, int64_t end)
{
    portENTER_CRITICAL(&s_lock);
    bool ours = s_sending || (start < s_quiet_us && end > s_tx_start_us);
    portEXIT_CRITICAL(&s_lock);
    return ours;
}

// Sends the frames one after another, each with its own carrier.
static void tx_task(void *arg)
{
    static frame_t f;
    uint32_t carrier = 0;
    for (;;) {
        xQueueReceive(s_tx_queue, &f, portMAX_DELAY);
        if (f.hz != carrier) {
            rmt_carrier_config_t c = {.frequency_hz = f.hz, .duty_cycle = 0.33f};
            if (rmt_apply_carrier(s_tx, &c) != ESP_OK) {
                link_log("warn", "infrared carrier %lu Hz not set", (unsigned long)f.hz);
                continue;
            }
            carrier = f.hz;
        }
        int n = 0;
        for (int i = 0; i < f.count; i += 2) {
            // An odd frame ends on a mark; a short space closes the symbol,
            // as a zero duration would end the transmission early.
            uint16_t space = i + 1 < f.count ? f.raw[i + 1] : 100;
            s_tx_symbols[n++] = (rmt_symbol_word_t){
                .level0 = 1, .duration0 = f.raw[i], .level1 = 0, .duration1 = space};
        }
        rmt_transmit_config_t tc = {.loop_count = 0};
        sending(true);
        esp_err_t err = rmt_transmit(s_tx, s_copy, s_tx_symbols, n * sizeof(rmt_symbol_word_t), &tc);
        if (err == ESP_OK) {
            err = rmt_tx_wait_all_done(s_tx, 1000);
        }
        sending(false);
        if (err != ESP_OK) {
            link_log("warn", "infrared send failed: %s", esp_err_to_name(err));
        }
    }
}

static bool on_received(rmt_channel_handle_t ch, const rmt_rx_done_event_data_t *e, void *ctx)
{
    BaseType_t woken = pdFALSE;
    size_t n = e->num_symbols;
    xQueueSendFromISR(s_rx_queue, &n, &woken);
    return woken == pdTRUE;
}

// Hands what the receiver picked up to go-dock, except our own frames.
static void rx_task(void *arg)
{
    static uint16_t raw[RX_SYMBOLS * 2];
    const rmt_receive_config_t rc = {.signal_range_min_ns = RX_GLITCH_NS, .signal_range_max_ns = RX_IDLE_NS};
    for (;;) {
        if (rmt_receive(s_rx, s_rx_symbols, sizeof(s_rx_symbols), &rc) != ESP_OK) {
            vTaskDelay(pdMS_TO_TICKS(1000));
            continue;
        }
        size_t symbols;
        xQueueReceive(s_rx_queue, &symbols, portMAX_DELAY);
        int count = 0;
        int64_t length = 0;
        for (size_t i = 0; i < symbols && count + 1 < (int)(sizeof(raw) / sizeof(raw[0])); i++) {
            // The input is inverted, so marks read high; zero ends the frame.
            const rmt_symbol_word_t *s = &s_rx_symbols[i];
            if (s->duration0 == 0) {
                break;
            }
            raw[count++] = s->duration0;
            length += s->duration0;
            if (s->duration1 == 0) {
                break;
            }
            raw[count++] = s->duration1;
            length += s->duration1;
        }
        if (count % 2 == 0 && count > 0) {
            length -= raw[--count]; // the frame ends on its last mark
        }
        int64_t end = esp_timer_get_time() - RX_IDLE_NS / 1000;
        if (count < 3 || echo(end - length, end)) {
            continue;
        }
        link_send_ir(raw, count);
    }
}

void ir_init(void)
{
    rmt_tx_channel_config_t tx = {
        .gpio_num = PIN_IR_TX,
        .clk_src = RMT_CLK_SRC_DEFAULT,
        .resolution_hz = RESOLUTION,
        .mem_block_symbols = 96,
        .trans_queue_depth = 1,
        .flags.invert_out = IR_TX_INVERT,
    };
    rmt_copy_encoder_config_t copy = {};
    rmt_carrier_config_t carrier = {.frequency_hz = 38000, .duty_cycle = 0.33f};
    esp_err_t err = rmt_new_tx_channel(&tx, &s_tx);
    if (err == ESP_OK) {
        err = rmt_apply_carrier(s_tx, &carrier);
    }
    if (err == ESP_OK) {
        err = rmt_new_copy_encoder(&copy, &s_copy);
    }
    if (err == ESP_OK) {
        err = rmt_enable(s_tx);
    }
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "transmitter not set up: %s", esp_err_to_name(err));
        s_tx = NULL;
        return;
    }
    s_tx_queue = xQueueCreate(4, sizeof(frame_t));
    xTaskCreate(tx_task, "ir_tx", 3072, NULL, 3, NULL);

    rmt_rx_channel_config_t rx = {
        .gpio_num = PIN_IR_RX,
        .clk_src = RMT_CLK_SRC_DEFAULT,
        .resolution_hz = RESOLUTION,
        .mem_block_symbols = RX_SYMBOLS,
        .flags.invert_in = 1, // the receiver pulls its output low while it sees the carrier
    };
    rmt_rx_event_callbacks_t cbs = {.on_recv_done = on_received};
    s_rx_queue = xQueueCreate(1, sizeof(size_t));
    err = rmt_new_rx_channel(&rx, &s_rx);
    if (err == ESP_OK) {
        // Nothing wired up: the pull-up keeps the line quiet.
        gpio_set_pull_mode(PIN_IR_RX, GPIO_PULLUP_ONLY);
        err = rmt_rx_register_event_callbacks(s_rx, &cbs, NULL);
    }
    if (err == ESP_OK) {
        err = rmt_enable(s_rx);
    }
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "receiver not set up: %s", esp_err_to_name(err));
        return;
    }
    xTaskCreate(rx_task, "ir_rx", 3072, NULL, 3, NULL);
}

bool ir_ready(void)
{
    return s_tx != NULL;
}

bool ir_send(uint32_t hz, const uint16_t *raw, int count)
{
    static frame_t f; // queued by copy; the link task is the only caller
    if (!s_tx || count <= 0 || count > IR_MAX_RAW) {
        return false;
    }
    f.hz = hz;
    f.count = count;
    memcpy(f.raw, raw, count * sizeof(*raw));
    return xQueueSend(s_tx_queue, &f, 0) == pdTRUE;
}
