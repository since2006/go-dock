package plugins

import (
	"testing"

	"github.com/since2006/go-dock/host/internal/model"
)

type fakePlugin []model.Reading

func (f fakePlugin) Readings() []model.Reading { return f }

func TestLine(t *testing.T) {
	h := New(fakePlugin{{ID: "a"}, {ID: "b"}}, fakePlugin(nil), fakePlugin{{ID: "c"}})
	line := h.Line()
	if len(line) != 3 || line[0].ID != "a" || line[2].ID != "c" {
		t.Fatalf("line %+v", line)
	}
	if New().Line() == nil {
		t.Fatal("no plugins should give an empty line, not nil")
	}
}
