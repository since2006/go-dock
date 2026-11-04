// Several go-docks share the screen, one page each, taking turns
// (docs/protocol-v1.md §7.1, «多台电脑»). This keeps what each one's tasks
// did lately and picks whose page shows. It has no ESP-IDF parts, so the
// simulator runs it too.
#pragma once

#include <stdbool.h>
#include <stdint.h>

#include "link.h"

#define HOST_DWELL_US (30 * 1000000LL) // a page stays this long before news elsewhere takes over
#define HOST_QUICK_US (5 * 1000000LL)  // only this long for news waiting for the user, or on an empty page
#define HOST_TURN_US (60 * 1000000LL)  // with nothing new, the pages with tasks take turns this often
#define HOST_HOLD_US (30 * 1000000LL)  // no turning this long after a touch
#define HOST_GONE_US (15 * 1000000LL)  // a go-dock this quiet has no page

// What a go-dock's last snapshot held, to tell news from the same again.
typedef struct {
    int count;
    struct {
        uint32_t id; // hashed session id
        dock_state_t state;
        int64_t since;
    } m[DOCK_MAX_SESSIONS];
} host_marks_t;

// host_note sums up snapshot s from h and notes whether it brought news: a
// task that is new (not idle), or one that came to 需授权, 等待输入, 出错
// or 已完成. Going back to work after an answer is no news.
void host_note(link_host_t *h, host_marks_t *marks, const dock_snapshot_t *s, int64_t now_us);

// host_present says whether h has a page: it speaks this protocol, sent a
// snapshot and was heard from lately.
bool host_present(const link_host_t *h, int64_t now_us);

// host_pick returns the slot whose page to show, -1 for none. shown is the
// slot on screen (-1 none), since shown_at; seen_us[i] when slot i was last
// on screen; touched_us the last touch. The page stays while it is touched.
// News elsewhere takes over after HOST_DWELL_US, or HOST_QUICK_US when it
// waits for the user and nothing on screen does, or the page on screen has
// no tasks. Without news the pages with tasks take turns every
// HOST_TURN_US (HOST_QUICK_US from an empty page); while the page on screen
// waits for the user, only pages that wait too. A page whose go-dock is gone
// gives way at once.
int host_pick(const link_host_t *h, const int64_t *seen_us, int shown, int64_t shown_at, int64_t touched_us,
              int64_t now_us);
