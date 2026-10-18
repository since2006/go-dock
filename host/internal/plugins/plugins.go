// Package plugins runs the function pages shown next to the Agent page.
// A plugin keeps its own state, describes its page with the generic
// widgets in model.Page, and handles taps on them; the screen and the web
// preview only draw what it describes.
//
// Adding one: implement Plugin in internal/plugins/<name>, give it a
// config section, and register it in internal/app.
package plugins

import (
	"context"
	"errors"

	"github.com/since2006/go-dock/host/internal/model"
)

// Plugin is one function page.
type Plugin interface {
	// ID names the page in the protocol: [a-z0-9_-], at most 16 bytes.
	ID() string
	// Page describes the page now; false hides it (disabled, nothing
	// configured). It is called from the device link and must not block.
	Page() (model.Page, bool)
	// Do handles a tap on item. It may block on the network until ctx
	// ends; the result shows up in the next Page.
	Do(ctx context.Context, item, act string) error
}

var (
	ErrNotFound = errors.New("没有这个功能页或控件")
	ErrBadAct   = errors.New("不支持的操作")
)

// Host holds the plugins in page order.
type Host struct {
	plugins []Plugin
}

func New(ps ...Plugin) *Host {
	return &Host{plugins: ps}
}

// Pages lists the visible pages in order.
func (h *Host) Pages() []model.Page {
	pages := []model.Page{}
	for _, p := range h.plugins {
		pg, ok := p.Page()
		if !ok {
			continue
		}
		pg.ID = p.ID()
		pages = append(pages, pg)
	}
	return pages
}

// Do routes a tap to the plugin owning the page.
func (h *Host) Do(ctx context.Context, a model.Action) error {
	for _, p := range h.plugins {
		if p.ID() == a.Page {
			if _, ok := p.Page(); !ok {
				return ErrNotFound
			}
			return p.Do(ctx, a.Item, a.Act)
		}
	}
	return ErrNotFound
}
