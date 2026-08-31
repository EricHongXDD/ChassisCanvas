/*
 * SPDX-FileCopyrightText: 2026 CaseScreen contributors
 *
 * SPDX-License-Identifier: CC0-1.0
 *
 * 受控实验骨架：ESP32-S3 作为 USB Host 连接微雪 RNDIS Gadget，
 * 再把该私网通过 Wi-Fi STA 和 IPv4 NAPT 接入上游网络。
 */

#include <stdbool.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/event_groups.h"

#include "esp_check.h"
#include "esp_event.h"
#include "esp_log.h"
#include "esp_netif.h"
#include "esp_wifi.h"
#include "nvs_flash.h"

#include "iot_eth.h"
#include "iot_eth_netif_glue.h"
#include "iot_usbh_cdc.h"
#include "iot_usbh_rndis.h"

#define CASESCREEN_WIFI_READY_BIT BIT0

#define CASESCREEN_USB_VENDOR_ID  0x1D6B
#define CASESCREEN_USB_PRODUCT_ID 0x0104

/* 与屏端 bridge-client 门禁保持一致。 */
#define CASESCREEN_BRIDGE_ADDRESS  ESP_IP4TOADDR(192, 168, 77, 1)
#define CASESCREEN_BRIDGE_NETMASK  ESP_IP4TOADDR(255, 255, 255, 0)

/* ESP-IDF release/v5.5 的 softap_sta 官方示例使用该值开启 DHCP DNS OFFER。 */
#define CASESCREEN_DHCPS_OFFER_DNS 0x02

static const char *TAG = "casescreen_bridge";

static EventGroupHandle_t s_wifi_events;
static esp_netif_t *s_wifi_sta_netif;
static esp_netif_t *s_rndis_netif;
static iot_eth_driver_t *s_rndis_driver;
static iot_eth_handle_t s_rndis_eth;
static iot_eth_netif_glue_handle_t s_rndis_glue;
static bool s_napt_enabled;

static const esp_netif_ip_info_t s_rndis_ip = {
    .ip.addr = CASESCREEN_BRIDGE_ADDRESS,
    .gw.addr = CASESCREEN_BRIDGE_ADDRESS,
    .netmask.addr = CASESCREEN_BRIDGE_NETMASK,
};

static esp_err_t set_napt(bool enabled)
{
    if (s_rndis_netif == NULL || s_napt_enabled == enabled) {
        return ESP_OK;
    }

    esp_err_t err = enabled
        ? esp_netif_napt_enable(s_rndis_netif)
        : esp_netif_napt_disable(s_rndis_netif);
    if (err == ESP_OK) {
        s_napt_enabled = enabled;
    }
    return err;
}

static esp_err_t copy_upstream_dns_to_dhcp(void)
{
    ESP_RETURN_ON_FALSE(s_wifi_sta_netif != NULL && s_rndis_netif != NULL,
                        ESP_ERR_INVALID_STATE, TAG, "network interfaces are not ready");

    esp_netif_dns_info_t dns = {0};
    ESP_RETURN_ON_ERROR(esp_netif_get_dns_info(s_wifi_sta_netif, ESP_NETIF_DNS_MAIN, &dns),
                        TAG, "failed to read Wi-Fi DNS");
    ESP_RETURN_ON_FALSE(dns.ip.u_addr.ip4.addr != 0,
                        ESP_ERR_INVALID_STATE, TAG, "Wi-Fi did not provide an IPv4 DNS server");

    esp_netif_dhcp_status_t old_status = ESP_NETIF_DHCP_INIT;
    ESP_RETURN_ON_ERROR(esp_netif_dhcps_get_status(s_rndis_netif, &old_status),
                        TAG, "failed to query DHCP server state");

    if (old_status == ESP_NETIF_DHCP_STARTED) {
        ESP_RETURN_ON_ERROR(esp_netif_dhcps_stop(s_rndis_netif),
                            TAG, "failed to pause DHCP server");
    }

    uint8_t offer_dns = CASESCREEN_DHCPS_OFFER_DNS;
    esp_err_t err = esp_netif_dhcps_option(
        s_rndis_netif,
        ESP_NETIF_OP_SET,
        ESP_NETIF_DOMAIN_NAME_SERVER,
        &offer_dns,
        sizeof(offer_dns));
    if (err == ESP_OK) {
        err = esp_netif_set_dns_info(s_rndis_netif, ESP_NETIF_DNS_MAIN, &dns);
    }

    if (old_status == ESP_NETIF_DHCP_STARTED) {
        esp_err_t start_err = esp_netif_dhcps_start(s_rndis_netif);
        if (err == ESP_OK) {
            err = start_err;
        }
    }

    return err;
}

static esp_err_t reject_overlapping_upstream_subnet(void)
{
    esp_netif_ip_info_t upstream = {0};
    ESP_RETURN_ON_ERROR(esp_netif_get_ip_info(s_wifi_sta_netif, &upstream),
                        TAG, "failed to read Wi-Fi address");

    const uint32_t downstream_network = s_rndis_ip.ip.addr & s_rndis_ip.netmask.addr;
    const uint32_t upstream_network = upstream.ip.addr & upstream.netmask.addr;
    const bool upstream_host_is_downstream =
        (upstream.ip.addr & s_rndis_ip.netmask.addr) == downstream_network;
    const bool downstream_host_is_upstream =
        (s_rndis_ip.ip.addr & upstream.netmask.addr) == upstream_network;

    ESP_RETURN_ON_FALSE(!upstream_host_is_downstream && !downstream_host_is_upstream,
                        ESP_ERR_INVALID_STATE, TAG,
                        "Wi-Fi subnet overlaps 192.168.77.0/24");
    return ESP_OK;
}

static void refresh_forwarding_after_wifi_ip(void)
{
    if (s_rndis_netif == NULL) {
        return;
    }

    esp_err_t err = reject_overlapping_upstream_subnet();
    if (err == ESP_OK) {
        err = copy_upstream_dns_to_dhcp();
    }
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "refusing forwarding because DNS refresh failed: %s", esp_err_to_name(err));
        ESP_ERROR_CHECK_WITHOUT_ABORT(set_napt(false));
        return;
    }

    err = esp_netif_set_default_netif(s_wifi_sta_netif);
    if (err == ESP_OK) {
        err = set_napt(true);
    }
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "failed to enable Wi-Fi forwarding: %s", esp_err_to_name(err));
        ESP_ERROR_CHECK_WITHOUT_ABORT(set_napt(false));
    }
}

static void wifi_event_handler(void *arg, esp_event_base_t event_base,
                               int32_t event_id, void *event_data)
{
    (void)arg;
    (void)event_data;

    if (event_base == WIFI_EVENT && event_id == WIFI_EVENT_STA_START) {
        ESP_ERROR_CHECK_WITHOUT_ABORT(esp_wifi_connect());
        return;
    }

    if (event_base == WIFI_EVENT && event_id == WIFI_EVENT_STA_DISCONNECTED) {
        xEventGroupClearBits(s_wifi_events, CASESCREEN_WIFI_READY_BIT);
        ESP_ERROR_CHECK_WITHOUT_ABORT(set_napt(false));
        ESP_LOGW(TAG, "Wi-Fi disconnected; USB client remains private and forwarding is closed");
        ESP_ERROR_CHECK_WITHOUT_ABORT(esp_wifi_connect());
        return;
    }

    if (event_base == IP_EVENT && event_id == IP_EVENT_STA_GOT_IP) {
        const ip_event_got_ip_t *got_ip = (const ip_event_got_ip_t *)event_data;
        ESP_LOGI(TAG, "Wi-Fi got address " IPSTR, IP2STR(&got_ip->ip_info.ip));
        xEventGroupSetBits(s_wifi_events, CASESCREEN_WIFI_READY_BIT);
        refresh_forwarding_after_wifi_ip();
    }
}

static esp_err_t initialize_wifi_sta(void)
{
    const size_t ssid_length = strlen(CONFIG_CASESCREEN_WIFI_SSID);
    const size_t password_length = strlen(CONFIG_CASESCREEN_WIFI_PASSWORD);
    ESP_RETURN_ON_FALSE(ssid_length > 0 && ssid_length <= sizeof(((wifi_sta_config_t *)0)->ssid),
                        ESP_ERR_INVALID_ARG, TAG, "Wi-Fi SSID is empty or too long");
    ESP_RETURN_ON_FALSE(password_length == 0 ||
                        (password_length >= 8 && password_length < sizeof(((wifi_sta_config_t *)0)->password)),
                        ESP_ERR_INVALID_ARG, TAG, "Wi-Fi password must be empty or 8-63 bytes");

    s_wifi_sta_netif = esp_netif_create_default_wifi_sta();
    ESP_RETURN_ON_FALSE(s_wifi_sta_netif != NULL, ESP_ERR_NO_MEM, TAG, "failed to create Wi-Fi STA netif");

    wifi_init_config_t init_config = WIFI_INIT_CONFIG_DEFAULT();
    ESP_RETURN_ON_ERROR(esp_wifi_init(&init_config), TAG, "failed to initialize Wi-Fi");
    ESP_RETURN_ON_ERROR(esp_wifi_set_storage(WIFI_STORAGE_RAM), TAG, "failed to select RAM Wi-Fi storage");
    ESP_RETURN_ON_ERROR(esp_wifi_set_mode(WIFI_MODE_STA), TAG, "failed to select Wi-Fi STA mode");

    wifi_config_t wifi_config = {0};
    memcpy(wifi_config.sta.ssid, CONFIG_CASESCREEN_WIFI_SSID, ssid_length);
    memcpy(wifi_config.sta.password, CONFIG_CASESCREEN_WIFI_PASSWORD, password_length);
    wifi_config.sta.scan_method = WIFI_ALL_CHANNEL_SCAN;
    wifi_config.sta.failure_retry_cnt = CONFIG_CASESCREEN_WIFI_MAXIMUM_RETRY;
    wifi_config.sta.threshold.authmode = password_length == 0 ? WIFI_AUTH_OPEN : WIFI_AUTH_WPA2_PSK;
    wifi_config.sta.sae_pwe_h2e = WPA3_SAE_PWE_BOTH;

    ESP_RETURN_ON_ERROR(esp_wifi_set_config(WIFI_IF_STA, &wifi_config), TAG, "failed to configure Wi-Fi STA");
    return esp_wifi_start();
}

static void rndis_event_handler(void *arg, esp_event_base_t event_base,
                                int32_t event_id, void *event_data)
{
    (void)arg;
    (void)event_base;
    (void)event_data;

    if (event_id == IOT_ETH_EVENT_CONNECTED) {
        ESP_LOGI(TAG, "Waveshare RNDIS link connected");
    } else if (event_id == IOT_ETH_EVENT_DISCONNECTED) {
        ESP_LOGW(TAG, "Waveshare RNDIS link disconnected");
    }
}

static esp_err_t install_rndis_downstream(void)
{
    usb_device_match_id_t *match_ids = calloc(2, sizeof(*match_ids));
    ESP_RETURN_ON_FALSE(match_ids != NULL, ESP_ERR_NO_MEM, TAG, "failed to allocate USB match list");

    match_ids[0].match_flags = USB_DEVICE_ID_MATCH_VID_PID;
    match_ids[0].idVendor = CASESCREEN_USB_VENDOR_ID;
    match_ids[0].idProduct = CASESCREEN_USB_PRODUCT_ID;

    const iot_usbh_rndis_config_t rndis_config = {
        .match_id_list = match_ids,
    };
    esp_err_t err = iot_eth_new_usb_rndis(&rndis_config, &s_rndis_driver);
    if (err != ESP_OK) {
        free(match_ids);
        return err;
    }
    /* 创建成功后，match_ids 的生命周期由官方 RNDIS 驱动管理。 */

    const iot_eth_config_t eth_config = {
        .driver = s_rndis_driver,
        .stack_input = NULL,
    };
    ESP_RETURN_ON_ERROR(iot_eth_install(&eth_config, &s_rndis_eth), TAG, "failed to install RNDIS Ethernet");

    const esp_netif_inherent_config_t inherent = {
        .flags = ESP_NETIF_DHCP_SERVER | ESP_NETIF_FLAG_AUTOUP,
        .ip_info = &s_rndis_ip,
        .get_ip_event = 0,
        .lost_ip_event = 0,
        .if_key = "CASESCREEN_RNDIS",
        .if_desc = "casescreen-rndis",
        .route_prio = 10,
    };
    const esp_netif_config_t netif_config = {
        .base = &inherent,
        .driver = NULL,
        .stack = ESP_NETIF_NETSTACK_DEFAULT_ETH,
    };
    s_rndis_netif = esp_netif_new(&netif_config);
    ESP_RETURN_ON_FALSE(s_rndis_netif != NULL, ESP_ERR_NO_MEM, TAG, "failed to create RNDIS netif");

    s_rndis_glue = iot_eth_new_netif_glue(s_rndis_eth);
    ESP_RETURN_ON_FALSE(s_rndis_glue != NULL, ESP_ERR_NO_MEM, TAG, "failed to create RNDIS netif glue");
    ESP_RETURN_ON_ERROR(esp_netif_attach(s_rndis_netif, s_rndis_glue), TAG, "failed to attach RNDIS netif");

    ESP_RETURN_ON_ERROR(iot_eth_start(s_rndis_eth), TAG, "failed to start RNDIS Ethernet");
    ESP_RETURN_ON_ERROR(reject_overlapping_upstream_subnet(), TAG, "unsafe Wi-Fi subnet");
    ESP_RETURN_ON_ERROR(copy_upstream_dns_to_dhcp(), TAG, "failed to configure downstream DNS");
    ESP_RETURN_ON_ERROR(esp_netif_set_default_netif(s_wifi_sta_netif), TAG, "failed to select Wi-Fi default route");
    return set_napt(true);
}

void app_main(void)
{
#ifndef CONFIG_CASESCREEN_RNDIS_FIRST_VERIFIED
    ESP_LOGE(TAG, "refusing to start: RNDIS-first descriptor and serial regression are not acknowledged");
    ESP_LOGE(TAG, "this scaffold is not compatible with the current ACM-first beta image");
    return;
#endif

    esp_err_t err = nvs_flash_init();
    if (err == ESP_ERR_NVS_NO_FREE_PAGES || err == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        err = nvs_flash_init();
    }
    ESP_ERROR_CHECK(err);
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());

    s_wifi_events = xEventGroupCreate();
    ESP_ERROR_CHECK(s_wifi_events != NULL ? ESP_OK : ESP_ERR_NO_MEM);
    ESP_ERROR_CHECK(esp_event_handler_instance_register(
        WIFI_EVENT, ESP_EVENT_ANY_ID, wifi_event_handler, NULL, NULL));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(
        IP_EVENT, IP_EVENT_STA_GOT_IP, wifi_event_handler, NULL, NULL));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(
        IOT_ETH_EVENT, ESP_EVENT_ANY_ID, rndis_event_handler, NULL, NULL));

    ESP_ERROR_CHECK(initialize_wifi_sta());
    xEventGroupWaitBits(s_wifi_events, CASESCREEN_WIFI_READY_BIT,
                        pdFALSE, pdTRUE, portMAX_DELAY);

    const usbh_cdc_driver_config_t cdc_config = {
        .task_stack_size = 4096,
        .task_priority = configMAX_PRIORITIES - 1,
        .task_coreid = 0,
        .skip_init_usb_host_driver = false,
    };
    ESP_ERROR_CHECK(usbh_cdc_driver_install(&cdc_config));
    ESP_ERROR_CHECK(install_rndis_downstream());

    ESP_LOGI(TAG, "experimental bridge ready: gateway 192.168.77.1/24");
}
