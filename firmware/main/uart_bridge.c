/*
 * uart_bridge.c — UART1 bridge for the wireless serial adapter.
 *
 * Data path:
 *   target TX -> UART1 RX -> driver ring buffer -> RX task -> 8 KB stream
 *   buffer -> (WSP DATA / RAW socket / WebSocket binary frame)
 *
 *   TCP/WSP -> uart_bridge_write() -> UART driver TX ring buffer -> target RX
 *
 * The UART format can be changed at runtime (UART_CFG); before switching we
 * wait for the TX FIFO to drain and drop both RX buffers.  All access to the
 * UART configuration is serialized with a mutex.
 */

#include "uart_bridge.h"

#include <inttypes.h>
#include <string.h>

#include "driver/gpio.h"
#include "driver/uart.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/stream_buffer.h"
#include "freertos/task.h"
#include "sdkconfig.h"

#include "wsp_proto.h"

#define UART_BRIDGE_PORT UART_NUM_1
#define UART_BRIDGE_TAG  "uart_bridge"

/* Target control lines (active low), -1 disables the mapping. */
#define PIN_RESET ((int)CONFIG_WSP_RESET_GPIO)
#define PIN_BOOT  ((int)CONFIG_WSP_BOOT_GPIO)

#define UART_RX_TASK_STACK 3072
#define UART_RX_TASK_PRIO  10
#define UART_TX_WAIT_MS    500 /* max wait for the FIFO to drain on reconfig */

static StreamBufferHandle_t s_rx_sb;
static SemaphoreHandle_t s_cfg_lock;
static portMUX_TYPE s_stat_mux = portMUX_INITIALIZER_UNLOCKED;
static uart_cfg_t s_cfg;
static bool s_line_dtr;
static bool s_line_rts;
static bool s_break_active;
static struct {
    uint32_t rx_bytes;
    uint32_t tx_bytes;
    uint32_t overflows;
} s_stats;

/* ------------------------------------------------------------------ */
/* Helpers                                                            */
/* ------------------------------------------------------------------ */

bool uart_bridge_cfg_valid(const uart_cfg_t *cfg)
{
    if (cfg == NULL) {
        return false;
    }
    if (cfg->baud < 50u || cfg->baud > 5000000u) {
        return false;
    }
    if (cfg->data_bits < 5u || cfg->data_bits > 8u) {
        return false;
    }
    if (cfg->parity > 2u) {
        return false;
    }
    if (cfg->stop_bits < 1u || cfg->stop_bits > 2u) {
        return false;
    }
    if (cfg->flow > 1u) {
        return false;
    }
    if (cfg->flow == 1u &&
        ((int)CONFIG_WSP_UART_RTS_GPIO < 0 || (int)CONFIG_WSP_UART_CTS_GPIO < 0)) {
        return false;
    }
    return true;
}

static uart_word_length_t map_data_bits(uint8_t data_bits)
{
    switch (data_bits) {
    case 5: return UART_DATA_5_BITS;
    case 6: return UART_DATA_6_BITS;
    case 7: return UART_DATA_7_BITS;
    default: return UART_DATA_8_BITS;
    }
}

static uart_parity_t map_parity(uint8_t parity)
{
    switch (parity) {
    case 1: return UART_PARITY_ODD;
    case 2: return UART_PARITY_EVEN;
    default: return UART_PARITY_DISABLE;
    }
}

static uart_stop_bits_t map_stop_bits(uint8_t stop_bits)
{
    return (stop_bits == 2u) ? UART_STOP_BITS_2 : UART_STOP_BITS_1;
}

static esp_err_t init_output_pin(int pin, int level)
{
    if (pin < 0) {
        return ESP_OK;
    }
    if (pin > 21) {
        ESP_LOGE(UART_BRIDGE_TAG, "GPIO%d is out of range for ESP32-C3", pin);
        return ESP_ERR_INVALID_ARG;
    }
    const gpio_config_t io = {
        .pin_bit_mask = (1ULL << (uint32_t)pin),
        .mode = GPIO_MODE_OUTPUT,
        .pull_up_en = GPIO_PULLUP_DISABLE,
        .pull_down_en = GPIO_PULLDOWN_DISABLE,
        .intr_type = GPIO_INTR_DISABLE,
    };
    esp_err_t err = gpio_config(&io);
    if (err != ESP_OK) {
        ESP_LOGE(UART_BRIDGE_TAG, "gpio_config(%d) failed: %s", pin, esp_err_to_name(err));
        return err;
    }
    return gpio_set_level((gpio_num_t)pin, level);
}

/* Apply a new configuration.  The caller must hold s_cfg_lock. */
static esp_err_t apply_locked(const uart_cfg_t *cfg)
{
    esp_err_t err = uart_wait_tx_done(UART_BRIDGE_PORT, pdMS_TO_TICKS(UART_TX_WAIT_MS));
    if (err != ESP_OK) {
        ESP_LOGW(UART_BRIDGE_TAG, "TX not drained before reconfig: %s", esp_err_to_name(err));
    }
    uart_flush_input(UART_BRIDGE_PORT);
    if (s_rx_sb != NULL) {
        (void)xStreamBufferReset(s_rx_sb);
    }

    const uart_config_t hw_cfg = {
        .baud_rate = (int)cfg->baud,
        .data_bits = map_data_bits(cfg->data_bits),
        .parity = map_parity(cfg->parity),
        .stop_bits = map_stop_bits(cfg->stop_bits),
        .flow_ctrl = (cfg->flow == 1u) ? UART_HW_FLOWCTRL_CTS_RTS : UART_HW_FLOWCTRL_DISABLE,
        .rx_flow_ctrl_thresh = (cfg->flow == 1u) ? 122 : 0,
        .source_clk = UART_SCLK_DEFAULT,
    };

    err = uart_param_config(UART_BRIDGE_PORT, &hw_cfg);
    if (err != ESP_OK) {
        ESP_LOGE(UART_BRIDGE_TAG, "uart_param_config failed: %s", esp_err_to_name(err));
        return err;
    }

    const int rts_pin = (cfg->flow == 1u) ? (int)CONFIG_WSP_UART_RTS_GPIO : UART_PIN_NO_CHANGE;
    const int cts_pin = (cfg->flow == 1u) ? (int)CONFIG_WSP_UART_CTS_GPIO : UART_PIN_NO_CHANGE;
    err = uart_set_pin(UART_BRIDGE_PORT, (int)CONFIG_WSP_UART_TX_GPIO,
                       (int)CONFIG_WSP_UART_RX_GPIO, rts_pin, cts_pin);
    if (err != ESP_OK) {
        ESP_LOGE(UART_BRIDGE_TAG, "uart_set_pin failed: %s", esp_err_to_name(err));
        return err;
    }

    err = uart_set_hw_flow_ctrl(UART_BRIDGE_PORT, hw_cfg.flow_ctrl,
                                hw_cfg.rx_flow_ctrl_thresh);
    if (err != ESP_OK) {
        ESP_LOGE(UART_BRIDGE_TAG, "uart_set_hw_flow_ctrl failed: %s", esp_err_to_name(err));
        return err;
    }

    s_cfg = *cfg;
    ESP_LOGI(UART_BRIDGE_TAG, "UART1: %" PRIu32 " %u%c%u flow=%u (TX=GPIO%d RX=GPIO%d)",
             cfg->baud, cfg->data_bits,
             (cfg->parity == 1u) ? 'O' : ((cfg->parity == 2u) ? 'E' : 'N'),
             cfg->stop_bits, cfg->flow, (int)CONFIG_WSP_UART_TX_GPIO,
             (int)CONFIG_WSP_UART_RX_GPIO);
    return ESP_OK;
}

/* ------------------------------------------------------------------ */
/* RX pump                                                            */
/* ------------------------------------------------------------------ */

static void uart_rx_task(void *arg)
{
    (void)arg;
    uint8_t buf[256];

    for (;;) {
        const int n = uart_read_bytes(UART_BRIDGE_PORT, buf, sizeof(buf), pdMS_TO_TICKS(20));
        if (n <= 0) {
            continue;
        }
        size_t sent = 0;
        if (s_rx_sb != NULL) {
            sent = xStreamBufferSend(s_rx_sb, buf, (size_t)n, 0);
        }
        portENTER_CRITICAL(&s_stat_mux);
        s_stats.rx_bytes += (uint32_t)n;
        s_stats.overflows += (uint32_t)((size_t)n - sent);
        portEXIT_CRITICAL(&s_stat_mux);
    }
}

/* ------------------------------------------------------------------ */
/* Public API                                                         */
/* ------------------------------------------------------------------ */

esp_err_t uart_bridge_init(const uart_cfg_t *initial)
{
    if (initial == NULL) {
        return ESP_ERR_INVALID_ARG;
    }

    uart_cfg_t cfg = *initial;
    if (!uart_bridge_cfg_valid(&cfg)) {
        ESP_LOGW(UART_BRIDGE_TAG, "invalid initial UART config, falling back to Kconfig defaults");
        cfg.baud = (uint32_t)CONFIG_WSP_DEFAULT_BAUD;
        cfg.data_bits = (uint8_t)CONFIG_WSP_DEFAULT_DATA_BITS;
        cfg.parity = (uint8_t)CONFIG_WSP_DEFAULT_PARITY;
        cfg.stop_bits = (uint8_t)CONFIG_WSP_DEFAULT_STOP_BITS;
        cfg.flow = 0;
    }

    s_rx_sb = xStreamBufferCreate(CONFIG_WSP_RX_BUFFER_SIZE, 1);
    if (s_rx_sb == NULL) {
        ESP_LOGE(UART_BRIDGE_TAG, "cannot create RX stream buffer");
        return ESP_ERR_NO_MEM;
    }
    s_cfg_lock = xSemaphoreCreateMutex();
    if (s_cfg_lock == NULL) {
        ESP_LOGE(UART_BRIDGE_TAG, "cannot create configuration mutex");
        vStreamBufferDelete(s_rx_sb);
        s_rx_sb = NULL;
        return ESP_ERR_NO_MEM;
    }

    const int rx_drv_buf = 2048;
    esp_err_t err = uart_driver_install(UART_BRIDGE_PORT, rx_drv_buf,
                                        CONFIG_WSP_TX_BUFFER_SIZE, 0, NULL, 0);
    if (err != ESP_OK) {
        ESP_LOGE(UART_BRIDGE_TAG, "uart_driver_install failed: %s", esp_err_to_name(err));
        return err;
    }

    err = apply_locked(&cfg);
    if (err != ESP_OK) {
        return err;
    }

    err = init_output_pin(PIN_RESET, 1); /* de-asserted (high) */
    if (err != ESP_OK) {
        ESP_LOGW(UART_BRIDGE_TAG, "reset GPIO init failed: %s", esp_err_to_name(err));
    }
    err = init_output_pin(PIN_BOOT, 1); /* de-asserted (high) */
    if (err != ESP_OK) {
        ESP_LOGW(UART_BRIDGE_TAG, "boot GPIO init failed: %s", esp_err_to_name(err));
    }

    BaseType_t ok = xTaskCreate(uart_rx_task, "uart_rx", UART_RX_TASK_STACK, NULL,
                                UART_RX_TASK_PRIO, NULL);
    if (ok != pdPASS) {
        ESP_LOGE(UART_BRIDGE_TAG, "cannot create UART RX task");
        return ESP_ERR_NO_MEM;
    }
    return ESP_OK;
}

esp_err_t uart_bridge_configure(const uart_cfg_t *req, uart_cfg_t *applied)
{
    if (req == NULL) {
        return ESP_ERR_INVALID_ARG;
    }
    if (!uart_bridge_cfg_valid(req)) {
        return ESP_ERR_INVALID_ARG;
    }
    if (s_cfg_lock == NULL) {
        return ESP_ERR_INVALID_STATE;
    }

    (void)xSemaphoreTake(s_cfg_lock, portMAX_DELAY);
    const esp_err_t err = apply_locked(req);
    (void)xSemaphoreGive(s_cfg_lock);

    if (err == ESP_OK && applied != NULL) {
        *applied = s_cfg;
    }
    return err;
}

void uart_bridge_get_cfg(uart_cfg_t *out)
{
    if (out == NULL) {
        return;
    }
    if (s_cfg_lock == NULL) {
        memset(out, 0, sizeof(*out));
        return;
    }
    (void)xSemaphoreTake(s_cfg_lock, portMAX_DELAY);
    *out = s_cfg;
    (void)xSemaphoreGive(s_cfg_lock);
}

esp_err_t uart_bridge_write(const uint8_t *data, size_t len, uint32_t timeout_ms)
{
    if (data == NULL || len == 0) {
        return ESP_ERR_INVALID_ARG;
    }

    const TickType_t start = xTaskGetTickCount();
    const TickType_t timeout_ticks = pdMS_TO_TICKS(timeout_ms);
    size_t off = 0;
    size_t queued = 0;

    while (off < len) {
        size_t free_size = 0;
        esp_err_t err = uart_get_tx_buffer_free_size(UART_BRIDGE_PORT, &free_size);
        if (err != ESP_OK) {
            return err;
        }
        if (free_size == 0) {
            if ((xTaskGetTickCount() - start) >= timeout_ticks) {
                if (queued > 0) {
                    portENTER_CRITICAL(&s_stat_mux);
                    s_stats.tx_bytes += (uint32_t)queued;
                    portEXIT_CRITICAL(&s_stat_mux);
                }
                return ESP_ERR_TIMEOUT;
            }
            vTaskDelay(pdMS_TO_TICKS(2));
            continue;
        }

        size_t chunk = len - off;
        if (chunk > free_size) {
            chunk = free_size;
        }
        const int written = uart_write_bytes(UART_BRIDGE_PORT,
                                             (const char *)(data + off), chunk);
        if (written < 0) {
            return ESP_FAIL;
        }
        off += (size_t)written;
        queued += (size_t)written;
    }

    portENTER_CRITICAL(&s_stat_mux);
    s_stats.tx_bytes += (uint32_t)queued;
    portEXIT_CRITICAL(&s_stat_mux);
    return ESP_OK;
}

esp_err_t uart_bridge_read(uint8_t *buf, size_t cap, uint32_t timeout_ms,
                           size_t *out_len)
{
    if (buf == NULL || cap == 0 || out_len == NULL || s_rx_sb == NULL) {
        return ESP_ERR_INVALID_ARG;
    }
    *out_len = 0;
    const TickType_t ticks = (timeout_ms == UINT32_MAX) ? portMAX_DELAY
                                                       : pdMS_TO_TICKS(timeout_ms);
    const size_t got = xStreamBufferReceive(s_rx_sb, buf, cap, ticks);
    if (got == 0) {
        return ESP_ERR_TIMEOUT;
    }
    *out_len = got;
    return ESP_OK;
}

void uart_bridge_flush_input(void)
{
    if (s_cfg_lock == NULL) {
        return;
    }
    (void)xSemaphoreTake(s_cfg_lock, portMAX_DELAY);
    uart_flush_input(UART_BRIDGE_PORT);
    if (s_rx_sb != NULL) {
        (void)xStreamBufferReset(s_rx_sb);
    }
    (void)xSemaphoreGive(s_cfg_lock);
}

esp_err_t uart_bridge_line_ctrl(bool dtr, bool rts, bool brk, uint8_t *state_out)
{
    if (PIN_BOOT >= 0) {
        (void)gpio_set_level((gpio_num_t)PIN_BOOT, dtr ? 0 : 1);
    }
    if (PIN_RESET >= 0) {
        (void)gpio_set_level((gpio_num_t)PIN_RESET, rts ? 0 : 1);
    }

    if (brk != s_break_active) {
        if (brk) {
            /* Let the current byte finish first, then hold TX low. */
            (void)uart_wait_tx_done(UART_BRIDGE_PORT, pdMS_TO_TICKS(200));
            (void)uart_set_line_inverse(UART_BRIDGE_PORT, UART_SIGNAL_TXD_INV);
        } else {
            (void)uart_set_line_inverse(UART_BRIDGE_PORT, UART_SIGNAL_INV_DISABLE);
        }
        s_break_active = brk;
    }

    s_line_dtr = dtr;
    s_line_rts = rts;
    if (state_out != NULL) {
        *state_out = uart_bridge_line_status();
    }
    return ESP_OK;
}

uint8_t uart_bridge_line_status(void)
{
    uint8_t state = 0;
    if (s_line_dtr) {
        state |= WSP_ST_DTR;
    }
    if (s_line_rts) {
        state |= WSP_ST_RTS;
    }
    if (s_break_active) {
        state |= WSP_ST_BREAK;
    }
    if (s_cfg.flow == 1u && (int)CONFIG_WSP_UART_CTS_GPIO >= 0) {
        if (gpio_get_level((gpio_num_t)CONFIG_WSP_UART_CTS_GPIO) != 0) {
            state |= WSP_ST_CTS;
        }
    }
    return state;
}

esp_err_t uart_bridge_reset_target(uint8_t action, uint16_t pulse_ms)
{
    if (PIN_RESET < 0) {
        return ESP_ERR_NOT_SUPPORTED;
    }

    switch (action) {
    case 0u: /* assert reset (low) */
        return gpio_set_level((gpio_num_t)PIN_RESET, 0);
    case 1u: /* release reset (high) */
        return gpio_set_level((gpio_num_t)PIN_RESET, 1);
    case 2u: { /* pulse */
        if (pulse_ms == 0u) {
            pulse_ms = 120u;
        }
        if (pulse_ms > 5000u) {
            return ESP_ERR_INVALID_ARG;
        }
        esp_err_t err = gpio_set_level((gpio_num_t)PIN_RESET, 0);
        if (err != ESP_OK) {
            return err;
        }
        vTaskDelay(pdMS_TO_TICKS(pulse_ms));
        return gpio_set_level((gpio_num_t)PIN_RESET, 1);
    }
    default:
        return ESP_ERR_INVALID_ARG;
    }
}

void uart_bridge_get_counters(uint32_t *rx_bytes, uint32_t *tx_bytes,
                              uint32_t *overflows)
{
    portENTER_CRITICAL(&s_stat_mux);
    const uint32_t rx = s_stats.rx_bytes;
    const uint32_t tx = s_stats.tx_bytes;
    const uint32_t ovf = s_stats.overflows;
    portEXIT_CRITICAL(&s_stat_mux);

    if (rx_bytes != NULL) {
        *rx_bytes = rx;
    }
    if (tx_bytes != NULL) {
        *tx_bytes = tx;
    }
    if (overflows != NULL) {
        *overflows = ovf;
    }
}
