idf_component_register(
    SRCS main.c display.c link.c ota.c ui.c fonts.c beep.c ir.c settings.c
    INCLUDE_DIRS .
    REQUIRES esp_driver_gpio esp_driver_i2c esp_driver_ledc esp_driver_rmt esp_driver_spi esp_driver_usb_serial_jtag
             esp_lcd esp_timer esp_app_format app_update mbedtls nvs_flash
    EMBED_FILES fonts/NotoSansSC-Regular.ttf fonts/NotoSansSC-Bold.ttf
                fonts/JetBrainsMono-Medium.ttf fonts/JetBrainsMono-Bold.ttf)
