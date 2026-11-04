#include "hosts.h"

#include <string.h>

static uint32_t hash(const char *s)
{
    uint32_t h = 2166136261u; // FNV-1a
    for (; *s; s++) {
        h = (h ^ (uint8_t)*s) * 16777619u;
    }
    return h;
}

void host_note(link_host_t *h, host_marks_t *marks, const dock_snapshot_t *s, int64_t now_us)
{
    static host_marks_t next; // the caller holds the link's receive lock
    bool news = false, urgent = false;
    next.count = 0;
    h->count = s->count;
    h->top = DOCK_STATE_COUNT;
    h->waiting = h->attention = 0;
    h->active = false;
    for (int i = 0; i < s->count; i++) {
        const dock_session_t *d = &s->sessions[i];
        uint32_t id = hash(d->id);
        bool known = false, same = false;
        for (int k = 0; k < marks->count; k++) {
            if (marks->m[k].id == id) {
                known = true;
                same = marks->m[k].state == d->state && marks->m[k].since == d->since;
                break;
            }
        }
        bool settled = d->state == DOCK_RUNNING || d->state == DOCK_IDLE;
        if ((!known && d->state != DOCK_IDLE) || (known && !same && !settled)) {
            news = true;
            urgent = urgent || d->state == DOCK_APPROVAL || d->state == DOCK_INPUT;
        }
        next.m[next.count].id = id;
        next.m[next.count].state = d->state;
        next.m[next.count].since = d->since;
        next.count++;

        if (d->state < h->top) {
            h->top = d->state; // the states run from the most urgent
        }
        if (d->state == DOCK_APPROVAL || d->state == DOCK_INPUT) {
            h->waiting++;
        }
        if (d->state == DOCK_APPROVAL || d->state == DOCK_INPUT || d->state == DOCK_ERROR) {
            h->attention++;
        }
        if (d->state != DOCK_DONE && d->state != DOCK_IDLE) {
            h->active = true;
        }
    }
    memcpy(marks, &next, sizeof(next));
    if (news) {
        h->news_us = now_us;
    }
    if (urgent) {
        h->urgent_us = now_us;
    }
}

bool host_present(const link_host_t *h, int64_t now_us)
{
    return h->gen && !h->incompatible && h->version && now_us - h->last_rx_us < HOST_GONE_US;
}

// better says whether candidate a beats b (-1 none) to come on screen:
// news waiting for the user, then any news, then a page with tasks, then
// the one off screen the longest (so pages without news take turns).
static bool better(const link_host_t *h, const int64_t *seen, int a, int b)
{
    if (b < 0) {
        return true;
    }
    bool ua = h[a].urgent_us > seen[a], ub = h[b].urgent_us > seen[b];
    if (ua != ub) {
        return ua;
    }
    bool na = h[a].news_us > seen[a], nb = h[b].news_us > seen[b];
    if (na != nb) {
        return na;
    }
    if ((h[a].count > 0) != (h[b].count > 0)) {
        return h[a].count > 0;
    }
    return seen[a] < seen[b];
}

int host_pick(const link_host_t *h, const int64_t *seen_us, int shown, int64_t shown_at, int64_t touched_us,
              int64_t now_us)
{
    bool live = shown >= 0 && host_present(&h[shown], now_us);
    if (live && now_us - touched_us < HOST_HOLD_US) {
        return shown; // someone is reading it
    }
    int best = -1;
    for (int i = 0; i < LINK_HOSTS; i++) {
        if (i == shown || !host_present(&h[i], now_us)) {
            continue;
        }
        if (live) {
            bool empty = h[shown].count == 0;
            int64_t wait;
            if (h[i].news_us > seen_us[i]) {
                bool quick = (h[i].urgent_us > seen_us[i] && !h[shown].waiting) || empty;
                wait = quick ? HOST_QUICK_US : HOST_DWELL_US;
            } else if (h[i].count > 0 && (h[i].waiting || !h[shown].waiting)) {
                wait = empty ? HOST_QUICK_US : HOST_TURN_US; // its turn
            } else {
                continue; // nothing to see there, or less than here
            }
            if (now_us - shown_at < wait) {
                continue;
            }
        }
        if (better(h, seen_us, i, best)) {
            best = i;
        }
    }
    return best >= 0 ? best : live ? shown : -1;
}
