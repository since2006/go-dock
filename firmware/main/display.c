// Display and touch for the Waveshare ESP32-S3-Touch-AMOLED-2.41 V2.
//
// V2 differs from V1: the panel and touch resets moved to the TCA9554 IO
// expander (EXIO0 / EXIO1), touch INT moved to GPIO3 and TE to GPIO21.
// The RM690B0 panel is driven with the SH8601 QSPI driver and Waveshare's
// init table; MADCTL 0x30 turns it to 600x450 landscape, which puts the
// 450 visible lines at RAM rows 16..465.
#include "display.h"

#include "driver/gpio.h"
#include "driver/i2c_master.h"
#include "driver/spi_master.h"
#include "esp_check.h"
#include "esp_heap_caps.h"
#include "esp_io_expander_tca9554.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_panel_ops.h"
#include "esp_lcd_sh8601.h"
#include "esp_lcd_touch_ft5x06.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lvgl.h"
#include "ui.h"

#define LCD_W 600
#define LCD_H 450
#define LCD_GAP_Y 16

#define PIN_LCD_CS 9
#define PIN_LCD_SCLK 10
#define PIN_LCD_D0 11
#define PIN_LCD_D1 12
#define PIN_LCD_D2 13
#define PIN_LCD_D3 14
#define PIN_I2C_SDA 47
#define PIN_I2C_SCL 48
#define PIN_TOUCH_INT 3
#define PIN_POWER_HOLD 16 // keeps the board powered on battery

#define EXIO_LCD_RESET IO_EXPANDER_PIN_NUM_0
#define EXIO_TOUCH_RESET IO_EXPANDER_PIN_NUM_1

#define LCD_HOST SPI2_HOST
#define BUF_LINES 40

static const char *TAG = "display";

static esp_lcd_panel_io_handle_t s_io;
static esp_lcd_panel_handle_t s_panel;
static esp_lcd_touch_handle_t s_touch;

static const sh8601_lcd_init_cmd_t INIT_CMDS[] = {
    {0xFE, (uint8_t[]){0x20}, 1, 0}, // manufacturer command page
    {0x26, (uint8_t[]){0x0A}, 1, 0},
    {0x24, (uint8_t[]){0x80}, 1, 0},
    {0xFE, (uint8_t[]){0x00}, 1, 0}, // back to user commands
    {0x3A, (uint8_t[]){0x55}, 1, 0}, // RGB565
    {0xC2, (uint8_t[]){0x00}, 1, 10},
    {0x35, (uint8_t[]){0x00}, 0, 0},  // tearing effect line on
    {0x51, (uint8_t[]){0x00}, 1, 10}, // brightness 0 until the UI is drawn
    {0x11, (uint8_t[]){0x00}, 0, 80}, // sleep out
    {0x2A, (uint8_t[]){0x00, 0x10, 0x01, 0xD1}, 4, 0},
    {0x2B, (uint8_t[]){0x00, 0x00, 0x02, 0x57}, 4, 0},
    {0x29, (uint8_t[]){0x00}, 0, 10}, // display on
    {0x36, (uint8_t[]){0x30}, 1, 0},  // MADCTL MV|ML: landscape
};

static uint32_t tick_ms(void)
{
    return (uint32_t)(esp_timer_get_time() / 1000);
}

static bool flush_done(esp_lcd_panel_io_handle_t io, esp_lcd_panel_io_event_data_t *edata, void *ctx)
{
    lv_display_flush_ready((lv_display_t *)ctx);
    return false;
}

static void flush(lv_display_t *disp, const lv_area_t *area, uint8_t *px)
{
    lv_draw_sw_rgb565_swap(px, lv_area_get_size(area)); // the panel wants big-endian RGB565
    esp_lcd_panel_draw_bitmap(s_panel, area->x1, area->y1, area->x2 + 1, area->y2 + 1, px);
}

// The controller only accepts windows that start and end on even pixels.
static void round_area(lv_event_t *e)
{
    lv_area_t *a = lv_event_get_param(e);
    a->x1 &= ~1;
    a->y1 &= ~1;
    a->x2 |= 1;
    a->y2 |= 1;
}

static void touch_read(lv_indev_t *indev, lv_indev_data_t *data)
{
    esp_lcd_touch_point_data_t pt;
    uint8_t count = 0;
    esp_lcd_touch_read_data(s_touch);
    if (esp_lcd_touch_get_data(s_touch, &pt, &count, 1) == ESP_OK && count > 0) {
        data->point.x = pt.x;
        data->point.y = pt.y;
        data->state = LV_INDEV_STATE_PRESSED;
        ui_touched();
    } else {
        data->state = LV_INDEV_STATE_RELEASED;
    }
}

static void pulse(esp_io_expander_handle_t exio, uint32_t pin, int low_ms, int high_ms)
{
    esp_io_expander_set_level(exio, pin, 0);
    vTaskDelay(pdMS_TO_TICKS(low_ms));
    esp_io_expander_set_level(exio, pin, 1);
    vTaskDelay(pdMS_TO_TICKS(high_ms));
}

void display_init(void)
{
    // Without this the board switches off when running on battery and the
    // power button is released.
    gpio_set_direction(PIN_POWER_HOLD, GPIO_MODE_OUTPUT);
    gpio_set_level(PIN_POWER_HOLD, 1);

    i2c_master_bus_handle_t i2c;
    i2c_master_bus_config_t i2c_cfg = {
        .i2c_port = I2C_NUM_0,
        .sda_io_num = PIN_I2C_SDA,
        .scl_io_num = PIN_I2C_SCL,
        .clk_source = I2C_CLK_SRC_DEFAULT,
        .glitch_ignore_cnt = 7,
        .flags.enable_internal_pullup = true,
    };
    ESP_ERROR_CHECK(i2c_new_master_bus(&i2c_cfg, &i2c));

    esp_io_expander_handle_t exio;
    ESP_ERROR_CHECK(esp_io_expander_new_i2c_tca9554(i2c, ESP_IO_EXPANDER_I2C_TCA9554_ADDRESS_000, &exio));
    esp_io_expander_set_dir(exio, EXIO_LCD_RESET | EXIO_TOUCH_RESET, IO_EXPANDER_OUTPUT);
    esp_io_expander_set_level(exio, EXIO_LCD_RESET | EXIO_TOUCH_RESET, 1);

    // Panel.
    const spi_bus_config_t bus = SH8601_PANEL_BUS_QSPI_CONFIG(PIN_LCD_SCLK, PIN_LCD_D0, PIN_LCD_D1, PIN_LCD_D2,
                                                              PIN_LCD_D3, LCD_W * BUF_LINES * sizeof(uint16_t));
    ESP_ERROR_CHECK(spi_bus_initialize(LCD_HOST, &bus, SPI_DMA_CH_AUTO));
    const esp_lcd_panel_io_spi_config_t io_cfg = SH8601_PANEL_IO_QSPI_CONFIG(PIN_LCD_CS, NULL, NULL);
    ESP_ERROR_CHECK(esp_lcd_new_panel_io_spi((esp_lcd_spi_bus_handle_t)LCD_HOST, &io_cfg, &s_io));

    sh8601_vendor_config_t vendor = {
        .init_cmds = INIT_CMDS,
        .init_cmds_size = sizeof(INIT_CMDS) / sizeof(INIT_CMDS[0]),
        .flags.use_qspi_interface = 1,
    };
    const esp_lcd_panel_dev_config_t panel_cfg = {
        .reset_gpio_num = -1, // reset is EXIO0, pulsed below
        .rgb_ele_order = LCD_RGB_ELEMENT_ORDER_RGB,
        .bits_per_pixel = 16,
        .vendor_config = &vendor,
    };
    ESP_ERROR_CHECK(esp_lcd_new_panel_sh8601(s_io, &panel_cfg, &s_panel));
    pulse(exio, EXIO_LCD_RESET, 20, 120);
    ESP_ERROR_CHECK(esp_lcd_panel_init(s_panel));
    ESP_ERROR_CHECK(esp_lcd_panel_set_gap(s_panel, 0, LCD_GAP_Y));

    // LVGL.
    lv_init();
    lv_tick_set_cb(tick_ms);
    lv_display_t *disp = lv_display_create(LCD_W, LCD_H);
    size_t buf_size = LCD_W * BUF_LINES * sizeof(uint16_t);
    void *buf1 = heap_caps_malloc(buf_size, MALLOC_CAP_DMA | MALLOC_CAP_INTERNAL);
    void *buf2 = heap_caps_malloc(buf_size, MALLOC_CAP_DMA | MALLOC_CAP_INTERNAL);
    assert(buf1 && buf2);
    lv_display_set_color_format(disp, LV_COLOR_FORMAT_RGB565);
    lv_display_set_buffers(disp, buf1, buf2, buf_size, LV_DISPLAY_RENDER_MODE_PARTIAL);
    lv_display_set_flush_cb(disp, flush);
    lv_display_add_event_cb(disp, round_area, LV_EVENT_INVALIDATE_AREA, NULL);
    const esp_lcd_panel_io_callbacks_t cbs = {.on_color_trans_done = flush_done};
    ESP_ERROR_CHECK(esp_lcd_panel_io_register_event_callbacks(s_io, &cbs, disp));

    // Touch: FT6336 reports portrait coordinates; swap and mirror them so
    // they match the landscape display.
    pulse(exio, EXIO_TOUCH_RESET, 10, 300);
    gpio_set_pull_mode(PIN_TOUCH_INT, GPIO_PULLUP_ONLY);
    esp_lcd_panel_io_handle_t tp_io;
    esp_lcd_panel_io_i2c_config_t tp_io_cfg = ESP_LCD_TOUCH_IO_I2C_FT5x06_CONFIG();
    tp_io_cfg.scl_speed_hz = 400 * 1000;
    ESP_ERROR_CHECK(esp_lcd_new_panel_io_i2c(i2c, &tp_io_cfg, &tp_io));
    const esp_lcd_touch_config_t tp_cfg = {
        .x_max = LCD_H - 1,
        .y_max = LCD_W - 1,
        .rst_gpio_num = GPIO_NUM_NC,
        .int_gpio_num = PIN_TOUCH_INT,
        .flags = {.swap_xy = 1, .mirror_x = 0, .mirror_y = 1},
    };
    if (esp_lcd_touch_new_i2c_ft5x06(tp_io, &tp_cfg, &s_touch) == ESP_OK) {
        lv_indev_t *indev = lv_indev_create();
        lv_indev_set_type(indev, LV_INDEV_TYPE_POINTER);
        lv_indev_set_read_cb(indev, touch_read);
        lv_indev_set_display(indev, disp);
    } else {
        ESP_LOGW(TAG, "touch controller not found; running without touch");
    }
}

void display_set_brightness(uint8_t level)
{
    uint32_t cmd = (0x02u << 24) | (0x51u << 8);
    esp_lcd_panel_io_tx_param(s_io, cmd, &level, 1);
}
