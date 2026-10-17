#include "fonts.h"

#include "esp_log.h"

extern const uint8_t noto_regular_start[] asm("_binary_NotoSansSC_Regular_ttf_start");
extern const uint8_t noto_regular_end[] asm("_binary_NotoSansSC_Regular_ttf_end");
extern const uint8_t noto_bold_start[] asm("_binary_NotoSansSC_Bold_ttf_start");
extern const uint8_t noto_bold_end[] asm("_binary_NotoSansSC_Bold_ttf_end");
extern const uint8_t mono_start[] asm("_binary_JetBrainsMono_Medium_ttf_start");
extern const uint8_t mono_end[] asm("_binary_JetBrainsMono_Medium_ttf_end");
extern const uint8_t mono_bold_start[] asm("_binary_JetBrainsMono_Bold_ttf_start");
extern const uint8_t mono_bold_end[] asm("_binary_JetBrainsMono_Bold_ttf_end");

// Glyphs kept rendered per font; CJK labels repeat few distinct characters.
#define GLYPH_CACHE 96
#define MAX_FONTS 32

typedef struct {
    font_face_t face;
    int size;
    lv_font_t *font;
} entry_t;

static entry_t s_fonts[MAX_FONTS];
static int s_count;

static lv_font_t *create(font_face_t face, int size)
{
    const uint8_t *start, *end;
    switch (face) {
    case FONT_BOLD:
        start = noto_bold_start, end = noto_bold_end;
        break;
    case FONT_MONO:
        start = mono_start, end = mono_end;
        break;
    case FONT_MONO_BOLD:
        start = mono_bold_start, end = mono_bold_end;
        break;
    default:
        start = noto_regular_start, end = noto_regular_end;
        break;
    }
    return lv_tiny_ttf_create_data_ex(start, end - start, size, LV_FONT_KERNING_NONE, GLYPH_CACHE);
}

const lv_font_t *font_get(font_face_t face, int size)
{
    for (int i = 0; i < s_count; i++) {
        if (s_fonts[i].face == face && s_fonts[i].size == size) {
            return s_fonts[i].font;
        }
    }
    if (s_count == MAX_FONTS) {
        ESP_LOGW("fonts", "font table full");
        return LV_FONT_DEFAULT;
    }
    lv_font_t *f = create(face, size);
    if (!f) {
        return LV_FONT_DEFAULT;
    }
    // Characters a face lacks come from Regular, which covers all of GB2312.
    if (face != FONT_REGULAR) {
        f->fallback = font_get(FONT_REGULAR, size);
    }
    s_fonts[s_count++] = (entry_t){face, size, f};
    return f;
}
