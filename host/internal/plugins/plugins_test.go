package plugins

import (
	"context"
	"errors"
	"testing"

	"github.com/since2006/go-dock/host/internal/model"
)

type fakePlugin struct {
	id     string
	shown  bool
	called []string
}

func (f *fakePlugin) ID() string { return f.id }

func (f *fakePlugin) Page() (model.Page, bool) {
	return model.Page{ID: "ignored", Title: f.id}, f.shown
}

func (f *fakePlugin) Do(ctx context.Context, item, act string) error {
	f.called = append(f.called, item+"/"+act)
	return nil
}

func TestHost(t *testing.T) {
	a, b, c := &fakePlugin{id: "a", shown: true}, &fakePlugin{id: "b"}, &fakePlugin{id: "c", shown: true}
	h := New(a, b, c)

	pages := h.Pages()
	if len(pages) != 2 || pages[0].ID != "a" || pages[1].ID != "c" {
		t.Fatalf("pages %+v", pages)
	}
	if err := h.Do(context.Background(), model.Action{Page: "c", Item: "x", Act: "on"}); err != nil {
		t.Fatal(err)
	}
	if len(c.called) != 1 || c.called[0] != "x/on" {
		t.Fatalf("called %v", c.called)
	}
	for _, page := range []string{"b", "zz"} {
		if err := h.Do(context.Background(), model.Action{Page: page, Item: "x", Act: "on"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("page %s: %v", page, err)
		}
	}
	if New().Pages() == nil {
		t.Fatal("no plugins should give an empty list, not nil")
	}
}
