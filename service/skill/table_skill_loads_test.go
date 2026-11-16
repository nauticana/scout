package skill

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/toolgateway"
)

var loadPrincipal = domain.Principal{Kind: domain.PrincipalAgent, ID: "wingmate", TenantID: 1, Release: "7"}

func TestSkillLoadsRecordAndCountTheReleasesRuns(t *testing.T) {
	ctx := context.Background()
	db := &skillLoadDB{answers: map[string][][]any{qSkillLoadRuns: {{int64(10), int64(4)}}}}
	loads := &TableSkillLoads{DB: db}
	if err := loads.Record(ctx, loadPrincipal, "req-1", "audit"); err != nil {
		t.Fatal(err)
	}
	if args := db.calls[qSkillLoadInsert][0]; args[0] != int64(1) || args[1] != "req-1" || args[2] != "audit" || args[3] != "wingmate" || args[4] != "7" {
		t.Fatalf("insert args = %v", args)
	}
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	count, err := loads.Runs(ctx, 1, "wingmate", "7", "audit", from, from.AddDate(0, 0, 7))
	if err != nil || count != (domain.SkillRunCount{Settled: 10, Loaded: 4}) {
		t.Fatalf("count = %+v, %v", count, err)
	}
	if args := db.calls[qSkillLoadRuns][0]; args[0] != "audit" || args[1] != int64(1) || args[2] != "wingmate" || args[3] != "7" {
		t.Fatalf("count args = %v", args)
	}
	for name, call := range map[string]func() error{
		"no request": func() error { return loads.Record(ctx, loadPrincipal, " ", "audit") },
		"no release": func() error {
			return loads.Record(ctx, domain.Principal{ID: "wingmate", TenantID: 1}, "req-1", "audit")
		},
		"empty window":   func() error { _, err := loads.Runs(ctx, 1, "wingmate", "7", "audit", from, from); return err },
		"no start":       func() error { _, err := loads.Runs(ctx, 1, "wingmate", "7", "audit", time.Time{}, from); return err },
		"no skill":       func() error { _, err := loads.Runs(ctx, 1, "wingmate", "7", "", from, from.Add(time.Hour)); return err },
		"negative purge": func() error { _, err := loads.Purge(ctx, -1, 10); return err },
		"no purge limit": func() error { _, err := loads.Purge(ctx, 30, 0); return err },
	} {
		if err := call(); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: want ErrValidation, got %v", name, err)
		}
	}
	db.err = errors.New("down")
	if _, err := loads.Runs(ctx, 1, "wingmate", "7", "audit", from, from.Add(time.Hour)); err == nil {
		t.Fatal("a failed count must surface")
	}
}

func TestSkillLoadsPurgeIsBoundedAndOffAtZeroDays(t *testing.T) {
	db := &skillLoadDB{answers: map[string][][]any{qSkillLoadPurge: {{"req-1"}, {"req-2"}}}}
	loads := &TableSkillLoads{DB: db}
	if purged, err := loads.Purge(context.Background(), 0, 10); err != nil || purged != 0 || len(db.calls) != 0 {
		t.Fatalf("zero days purged %d, %v", purged, err)
	}
	purged, err := loads.Purge(context.Background(), 30, 10)
	if err != nil || purged != 2 || db.calls[qSkillLoadPurge][0][0] != 30 || db.calls[qSkillLoadPurge][0][1] != 10 {
		t.Fatalf("purged %d, %v, args %v", purged, err, db.calls[qSkillLoadPurge])
	}
}

func TestUseSkillRecordsTheTurnsLoad(t *testing.T) {
	fake, registry := newRegistry()
	ctx := context.Background()
	if err := registry.Register(ctx, 1, auditSkill("1")); err != nil {
		t.Fatal(err)
	}
	if err := registry.WriteRelease(ctx, fake, 1, release([]string{domain.UseSkillToolID, "crawl", "propose_edit"}, domain.SkillReference{SkillID: "audit", Version: "1"})); err != nil {
		t.Fatal(err)
	}
	db := &skillLoadDB{}
	transport := &toolgateway.InProcessTransport{}
	if err := (&UseSkill{Skills: registry, Loads: &TableSkillLoads{DB: db}}).Register(transport); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"skill": "audit"})
	call := domain.ToolCall{Principal: loadPrincipal, RequestID: "req-1", Arguments: args}
	if _, err := transport.Invoke(ctx, call, UseSkillTool(), nil, 0); err != nil || len(db.calls[qSkillLoadInsert]) != 1 {
		t.Fatalf("use_skill = %v, loads %v", err, db.calls)
	}
	call.Principal.Release = "6"
	if _, err := transport.Invoke(ctx, call, UseSkillTool(), nil, 0); !errors.Is(err, domain.ErrNotFound) || len(db.calls[qSkillLoadInsert]) != 1 {
		t.Fatalf("an unbound skill = %v, loads %v", err, db.calls)
	}
	call.Principal.Release = "7"
	db.err = errors.New("down")
	if _, err := transport.Invoke(ctx, call, UseSkillTool(), nil, 0); err == nil {
		t.Fatal("a failed load record must fail the call")
	}
}
