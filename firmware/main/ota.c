#include "ota.h"

#include <stdio.h>
#include <string.h>

#include "esp_app_desc.h"
#include "esp_log.h"
#include "esp_ota_ops.h"
#include "esp_timer.h"
#include "link.h"
#include "mbedtls/base64.h"

#define PROBATION_US (120 * 1000000LL) // a new image must reach go-dock within this
#define IDLE_US (15 * 1000000LL)        // an update the host stopped feeding is dropped
#define SHOW_FAILED_US (5 * 1000000LL)  // how long the screen shows a failure
#define ASK_US (60 * 1000000LL)         // how long the screen asks before giving up

static const char *TAG = "ota";
static const char *PROJECT = "go-dock-firmware";

// The update in progress; only the reader task changes these.
static const esp_partition_t *s_part;
static esp_ota_handle_t s_handle;
static size_t s_size, s_written;
static char s_target[32];
static int64_t s_last_us;
static char s_error[96];
static int64_t s_failed_us;
static uint8_t s_buf[OTA_CHUNK]; // in internal RAM, which writes to flash faster
static bool s_wifi;              // the update came over WiFi

// The question before an update over WiFi; the UI task answers it.
static size_t s_ask_size;
static int64_t s_ask_until;
static char s_from[52];
static volatile int s_answer; // 1 yes, -1 no, 0 not yet

static volatile ota_state_t s_state;
static volatile int s_percent;

static esp_timer_handle_t s_probation;
static volatile bool s_pending; // running a new image not yet confirmed

static void probation_over(void *arg)
{
    ESP_LOGW(TAG, "new firmware never reached go-dock, rolling back");
    esp_ota_mark_app_invalid_rollback_and_reboot();
}

void ota_init(void)
{
    esp_ota_img_states_t st;
    if (esp_ota_get_state_partition(esp_ota_get_running_partition(), &st) != ESP_OK || st != ESP_OTA_IMG_PENDING_VERIFY) {
        return;
    }
    const esp_timer_create_args_t args = {.callback = probation_over, .name = "ota"};
    if (esp_timer_create(&args, &s_probation) == ESP_OK) {
        s_pending = true;
        esp_timer_start_once(s_probation, PROBATION_US);
    }
}

void ota_confirm(void)
{
    if (!s_pending) {
        return;
    }
    s_pending = false;
    esp_timer_stop(s_probation);
    esp_ota_mark_app_valid_cancel_rollback();
    link_log("info", "固件 %s 已确认", ota_version());
}

static const char *fail(const char *what, esp_err_t err)
{
    if (s_handle) {
        esp_ota_abort(s_handle);
        s_handle = 0;
    }
    if (err != ESP_OK) {
        snprintf(s_error, sizeof(s_error), "%s（%s）", what, esp_err_to_name(err));
    } else {
        snprintf(s_error, sizeof(s_error), "%s", what);
    }
    ESP_LOGW(TAG, "%s", s_error);
    s_failed_us = esp_timer_get_time();
    s_state = OTA_FAILED;
    return s_error;
}

// check finds the partition an update of size bytes goes to.
static const char *check(size_t size)
{
    s_part = esp_ota_get_next_update_partition(NULL);
    if (!s_part) {
        return fail("没有可用的升级分区", ESP_OK);
    }
    if (size == 0 || size > s_part->size) {
        return fail("固件大小超出分区", ESP_OK);
    }
    return NULL;
}

// start erases the spare partition for an update of size bytes to s_target.
static const char *start(size_t size)
{
    if (s_handle) { // a new update replaces an unfinished one
        esp_ota_abort(s_handle);
        s_handle = 0;
    }
    const char *bad = check(size);
    if (bad) {
        return bad;
    }
    s_size = size;
    s_written = 0;
    s_percent = 0;
    s_state = OTA_PREPARING;
    // Erases the whole image now: much faster than sector by sector.
    esp_err_t err = esp_ota_begin(s_part, size, &s_handle);
    if (err == ESP_ERR_OTA_ROLLBACK_INVALID_STATE) {
        return fail("当前固件还没确认，请重新连接 go-dock 后再试", ESP_OK);
    }
    if (err != ESP_OK) {
        s_handle = 0;
        return fail("擦除升级分区失败", err);
    }
    s_last_us = esp_timer_get_time();
    s_state = OTA_RECEIVING;
    ESP_LOGI(TAG, "receiving %s, %u bytes into %s", s_target, (unsigned)size, s_part->label);
    return NULL;
}

const char *ota_begin(size_t size, const char *version)
{
    snprintf(s_target, sizeof(s_target), "%s", version);
    s_wifi = false;
    return start(size);
}

const char *ota_ask(size_t size, const char *version, const char *from)
{
    if (s_handle) {
        esp_ota_abort(s_handle);
        s_handle = 0;
    }
    const char *err = check(size); // refused before anyone is asked
    if (err) {
        return err;
    }
    snprintf(s_target, sizeof(s_target), "%s", version);
    snprintf(s_from, sizeof(s_from), "%s", from);
    s_ask_size = size;
    s_ask_until = esp_timer_get_time() + ASK_US;
    s_answer = 0;
    s_wifi = true;
    s_state = OTA_ASKING;
    ESP_LOGI(TAG, "asking to update to %s from %s", s_target, s_from[0] ? s_from : "go-dock");
    return NULL;
}

void ota_answer(bool yes)
{
    if (s_state == OTA_ASKING && s_answer == 0) {
        s_answer = yes ? 1 : -1;
    }
}

int ota_asked(void)
{
    if (s_state != OTA_ASKING) {
        return 0;
    }
    int a = s_answer;
    if (a > 0) {
        return 1;
    }
    if (a < 0 || esp_timer_get_time() > s_ask_until) {
        s_state = OTA_IDLE;
        ESP_LOGI(TAG, "update %s", a < 0 ? "declined" : "not answered");
        return a < 0 ? -1 : -2;
    }
    return 0;
}

const char *ota_begin_asked(void)
{
    if (s_state != OTA_ASKING || s_answer <= 0) {
        return "没有待确认的升级";
    }
    return start(s_ask_size);
}

bool ota_busy(void)
{
    return s_state == OTA_ASKING || s_state == OTA_PREPARING || s_state == OTA_RECEIVING;
}

const char *ota_write(size_t off, const char *base64, size_t *written)
{
    if (s_state != OTA_RECEIVING) {
        return "没有进行中的升级";
    }
    if (off != s_written) {
        return fail("数据位置不对", ESP_OK);
    }
    size_t n = 0;
    if (mbedtls_base64_decode(s_buf, sizeof(s_buf), &n, (const unsigned char *)base64, strlen(base64)) != 0 || n == 0 ||
        s_written + n > s_size) {
        return fail("数据格式不对", ESP_OK);
    }
    esp_err_t err = esp_ota_write(s_handle, s_buf, n);
    if (err != ESP_OK) {
        return fail("写入失败", err);
    }
    s_written += n;
    s_last_us = esp_timer_get_time();
    s_percent = (int)(s_written * 100 / s_size);
    *written = s_written;
    return NULL;
}

const char *ota_finish(void)
{
    if (s_state != OTA_RECEIVING) {
        return "没有进行中的升级";
    }
    if (s_written != s_size) {
        return fail("数据不完整", ESP_OK);
    }
    esp_err_t err = esp_ota_end(s_handle); // checks the image's digest
    s_handle = 0;
    if (err != ESP_OK) {
        return fail("固件校验失败", err);
    }
    esp_app_desc_t desc;
    if (esp_ota_get_partition_description(s_part, &desc) != ESP_OK || strcmp(desc.project_name, PROJECT) != 0) {
        return fail("不是 go-dock 的固件", ESP_OK);
    }
    if (strcmp(desc.version, s_target) != 0) {
        return fail("固件版本和声明的不一致", ESP_OK);
    }
    err = esp_ota_set_boot_partition(s_part);
    if (err != ESP_OK) {
        return fail("切换启动分区失败", err);
    }
    s_state = OTA_RESTARTING;
    ESP_LOGI(TAG, "%s accepted, restarting", s_target);
    return NULL;
}

void ota_abort(const char *why)
{
    if (s_state == OTA_ASKING) {
        s_state = OTA_IDLE; // nothing started: the question just goes
    } else if (s_state == OTA_PREPARING || s_state == OTA_RECEIVING) {
        fail(why, ESP_OK);
    }
}

void ota_tick(void)
{
    if (s_state == OTA_RECEIVING && esp_timer_get_time() - s_last_us > IDLE_US) {
        fail("go-dock 停止了发送", ESP_OK);
    }
}

ota_state_t ota_status(int *percent, const char **error)
{
    ota_state_t st = s_state;
    if (st == OTA_FAILED && esp_timer_get_time() - s_failed_us > SHOW_FAILED_US) {
        st = OTA_IDLE;
    }
    if (st == OTA_ASKING && s_answer) { // answered; the link acts on it shortly
        st = s_answer > 0 ? OTA_PREPARING : OTA_IDLE;
    }
    if (percent) {
        *percent = s_percent;
    }
    if (error) {
        *error = st == OTA_FAILED ? s_error : "";
    }
    return st;
}

const char *ota_version(void)
{
    return esp_app_get_description()->version;
}

const char *ota_target(void)
{
    return s_target;
}

const char *ota_from(void)
{
    return s_from;
}

int ota_ask_left(void)
{
    int64_t left = s_ask_until - esp_timer_get_time();
    return left > 0 ? (int)((left + 999999) / 1000000) : 0;
}

bool ota_over_wifi(void)
{
    return s_wifi;
}
