idf_component_register(
    SRCS "casescreen_usb_bridge.c"
    INCLUDE_DIRS "."
    REQUIRES esp_event esp_netif esp_wifi nvs_flash
)
