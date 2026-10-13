package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sokingdom/toll/internal/events"
	"github.com/sokingdom/toll/internal/reqid"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "toll.db"), 31*24*time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ptr[T any](v T) *T { return &v }

func common(id, user, model string) events.Common {
	c := events.Common{RequestID: id, Provider: "openai",
		Endpoint: "chat", RequestedModel: ptr(model), Tags: map[string]string{"feature": "chat"}}
	if user != "" {
		c.User = ptr(user)
	}
	return c
}

func completed(c events.Common, at time.Time, status int, in, out int64) events.Completed {
	ev := events.Completed{Common: c, Status: status, UsageSource: events.UsageProvider, LatencyMS: 120}
	ev.TS = events.Timestamp(at)
	ev.Model = ptr(*c.RequestedModel + "-2024")
	ev.InputTokens, ev.OutputTokens = ptr(in), ptr(out)
	zero := int64(0)
	ev.CacheReadTokens, ev.CacheWriteTokens, ev.ReasoningTokens = &zero, &zero, &zero
	return ev
}

func TestStartedThenCompleted(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	at := time.Now()
	c := common(reqid.New(), "u1", "gpt-4o")

	s.Started(events.Started{Common: c}, at)
	s.Flush()
	open, err := s.Open(ctx, 10)
	if err != nil || len(open) != 1 || open[0].RequestID != c.RequestID {
		t.Fatalf("open = %+v, %v", open, err)
	}

	s.Completed(completed(c, at.Add(time.Second), 200, 10, 5), at)
	s.Flush()
	if open, _ := s.Open(ctx, 10); len(open) != 0 {
		t.Fatalf("still open: %+v", open)
	}
	list, err := s.List(ctx, Filter{})
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	r := list[0]
	if *r.User != "u1" || *r.Status != 200 || *r.InputTokens != 10 || *r.OutputTokens != 5 ||
		*r.Model != "gpt-4o-2024" || r.Tags["feature"] != "chat" {
		t.Fatalf("row = %+v", r)
	}
}

func TestCompletedWithoutStarted(t *testing.T) {
	s := open(t)
	c := common(reqid.New(), "", "gpt-4o")
	ev := events.Completed{Common: c, Status: 400, Rejected: true, Error: "user_required", UsageSource: events.UsageNone}
	ev.TS = events.Timestamp(time.Now())
	s.Completed(ev, time.Now())
	s.Flush()
	list, err := s.List(context.Background(), Filter{Status: "error"})
	if err != nil || len(list) != 1 || !list[0].Rejected || *list[0].Error != "user_required" || list[0].User != nil {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestListFiltersAndCursor(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	var ids []string
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		id := reqid.Floor(at)[:10] + reqid.New()[10:]
		ids = append(ids, id)
		user, status := "a", 200
		if i%2 == 1 {
			user, status = "b", 500
		}
		s.Completed(completed(common(id, user, "m"), at, status, 1, 1), at)
	}
	s.Flush()

	page, _ := s.List(ctx, Filter{Limit: 2})
	if len(page) != 2 || page[0].RequestID != ids[4] || page[1].RequestID != ids[3] {
		t.Fatalf("page 1: %+v", page)
	}
	page, _ = s.List(ctx, Filter{Limit: 2, Before: page[1].RequestID})
	if len(page) != 2 || page[0].RequestID != ids[2] {
		t.Fatalf("page 2: %+v", page)
	}
	if got, _ := s.List(ctx, Filter{User: "b"}); len(got) != 2 {
		t.Fatalf("user filter: %d", len(got))
	}
	if got, _ := s.List(ctx, Filter{Status: "ok"}); len(got) != 3 {
		t.Fatalf("status filter: %d", len(got))
	}
	if got, _ := s.List(ctx, Filter{Model: "m-2024", Since: base.Add(90 * time.Second)}); len(got) != 3 {
		t.Fatalf("model+since filter: %d", len(got))
	}

	var n int
	var out int64
	err := s.ScanUser(ctx, "a", base, base.Add(time.Hour), func(r UsageRow) { n++; out += r.Tokens[3] })
	if err != nil || n != 3 || out != 3 {
		t.Fatalf("scan: n=%d out=%d err=%v", n, out, err)
	}
}

func TestPrune(t *testing.T) {
	s := open(t)
	old := time.Now().Add(-40 * 24 * time.Hour)
	oldID := reqid.Floor(old)[:10] + reqid.New()[10:]
	s.Completed(completed(common(oldID, "u", "m"), old, 200, 1, 1), old)
	s.Completed(completed(common(reqid.New(), "u", "m"), time.Now(), 200, 1, 1), time.Now())
	s.Flush()
	s.prune()
	list, _ := s.List(context.Background(), Filter{})
	if len(list) != 1 || list[0].RequestID == oldID {
		t.Fatalf("after prune: %+v", list)
	}
	if st := s.Stats(context.Background()); st.Oldest == nil || st.SizeBytes == 0 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestReopenKeepsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toll.db")
	s, err := Open(path, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Started(events.Started{Common: common(reqid.New(), "u", "m")}, time.Now())
	s.Close() // flushes
	s, err = Open(path, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if open, _ := s.Open(context.Background(), 10); len(open) != 1 {
		t.Fatalf("open after reopen: %d", len(open))
	}
}
