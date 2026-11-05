package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/recording"
)

func TestShopPricingAndFailures(t *testing.T) {
	s := NewShop()
	if len(s.Menu()) != 5 {
		t.Fatal("menu size")
	}
	if _, _, err := s.QuoteDelivery("Lviv, Rynok 1"); err != ErrNotDeliverable {
		t.Fatalf("lviv: %v", err)
	}
	if fee, _, _ := s.QuoteDelivery("Obolon, Kyiv"); fee != 9000 {
		t.Fatalf("left bank fee %d", fee)
	}
	d, err := s.Propose("c1", []Line{{"pepperoni", Large, 2}, {"margherita", Small, 1}}, "Khreshchatyk 22, Kyiv", "")
	if err != nil {
		t.Fatal(err)
	}
	// 220*1.3*2 + 180*0.8 + 60 delivery
	if d.ItemsKop != 57200+14400 || d.TotalKop != 71600+6000 {
		t.Fatalf("draft %+v", d)
	}
	for _, bad := range [][]Line{nil, {{"sushi", Medium, 1}}, {{"veggie", "XL", 1}}, {{"veggie", Medium, 0}}} {
		if _, err := s.Propose("c2", bad, "Kyiv", ""); err == nil {
			t.Errorf("want error for %+v", bad)
		}
	}
	s.SetOutOfStock("pepperoni")
	if _, err := s.Propose("c2", []Line{{"pepperoni", Medium, 1}}, "Kyiv", ""); err == nil || !strings.Contains(err.Error(), "out of stock") {
		t.Fatalf("stock: %v", err)
	}

	if _, err := s.PlaceAndPay("nope", "", "tok_test_visa", ""); err == nil {
		t.Fatal("no draft")
	}
	s.DeclineNextCharge()
	if o, err := s.PlaceAndPay("c1", d.Hash(), "tok_test_visa", ""); err == nil || o.Status != "payment_failed" {
		t.Fatalf("decline: %+v %v", o, err)
	}
	o, err := s.PlaceAndPay("c1", d.Hash(), "tok_test_visa", "")
	if err != nil || o.Status != "paid" || o.ID != "ORD-0002" {
		t.Fatalf("pay: %+v %v", o, err)
	}
	if _, ok := s.Draft("c1"); ok {
		t.Fatal("draft must be consumed")
	}
	if !strings.Contains(d.Summary(s), "TOTAL 776.00 UAH") {
		t.Fatal(d.Summary(s))
	}
}

func TestPrinterAndReplay(t *testing.T) {
	dir := t.TempDir()
	w, err := recording.Dir(dir)(recording.Header{Label: "order-1", StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Looking"},{"type":"tool_use","id":"t1","name":"mcp__pizza__get_menu","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"5 pizzas"}]}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":"out of stock"}]}}`,
		`{"type":"result","num_turns":2,"total_cost_usd":0.01,"result":"ok"}`,
	} {
		w.WriteLine([]byte(l))
	}
	w.Close()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	j.Note(NewPrinter(&sink), "customer: hi")
	j.Close()

	var buf bytes.Buffer
	if err := Replay(&buf, dir, 0); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"customer: hi", "agent order-1", "💬 Looking", "🔧 get_menu {}", "↳ 5 pizzas", "✗ out of stock", "✓ agent done: 2 turns"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if err := Replay(&buf, t.TempDir(), 0); err == nil {
		t.Error("empty dir should error")
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	envf := filepath.Join(dir, "x.env")
	os.WriteFile(envf, []byte("# c\nPIZZA_MODEL=\"m1\"\nANTHROPIC_MODEL=keep\nAWS_SECRET_ACCESS_KEY=evil\nbad\n"), 0o600)
	t.Setenv("ANTHROPIC_MODEL", "env")
	t.Setenv("PIZZA_MODEL", "")
	os.Unsetenv("PIZZA_MODEL")
	os.Unsetenv("AWS_SECRET_ACCESS_KEY")
	path, err := LoadDotEnv(envf)
	if err != nil || path != envf {
		t.Fatalf("%s %v", path, err)
	}
	if os.Getenv("PIZZA_MODEL") != "m1" || os.Getenv("ANTHROPIC_MODEL") != "env" {
		t.Fatal("values")
	}
	if _, set := os.LookupEnv("AWS_SECRET_ACCESS_KEY"); set {
		t.Fatal("non-allowlisted key must not be loaded")
	}
	os.Unsetenv("PIZZA_MODEL")
}

func TestLoadDotEnvDoesNotWalkParents(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a", "b")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("PIZZA_MODEL=parent\n"), 0o600)
	os.Unsetenv("PIZZA_MODEL")
	t.Chdir(sub)
	// cwd .env absent; temp dir is outside the repo, so no root fallback either.
	path, err := LoadDotEnv("")
	if err != nil || path != "" {
		t.Fatalf("%q %v", path, err)
	}
	if os.Getenv("PIZZA_MODEL") != "" {
		t.Fatal("parent .env must not be read")
	}
}

func TestLoadDotEnvRepoRootFallback(t *testing.T) {
	// Run from the package dir: .env is absent in cwd, repo root has go.mod.
	root, ok := repoRoot()
	if !ok {
		t.Fatal("repo root not found from package dir")
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal(err)
	}
	if _, ok := repoRootFrom(t.TempDir()); ok {
		t.Fatal("temp dir is not inside the repo")
	}
}

func TestPlaceRefusesSwappedDraft(t *testing.T) {
	s := NewShop()
	d, err := s.Propose("c1", []Line{{"margherita", Small, 1}}, "Kyiv", "")
	if err != nil {
		t.Fatal(err)
	}
	confirmed := d.Hash()
	// Draft swapped after the human confirmed.
	if _, err := s.Propose("c1", []Line{{"pepperoni", Large, 10}}, "Kyiv", ""); err != nil {
		t.Fatal(err)
	}
	if o, err := s.PlaceAndPay("c1", confirmed, "tok_test_visa", ""); !errors.Is(err, ErrDraftChanged) || o.ID != "" {
		t.Fatalf("swap must be refused: %+v %v", o, err)
	}
	if len(s.Orders()) != 0 {
		t.Fatal("nothing may be placed")
	}
	if _, ok := s.Draft("c1"); !ok {
		t.Fatal("draft must stay")
	}
}

func TestNewCartID128Bit(t *testing.T) {
	id := newCartID()
	if len(id) != len("cart-")+32 || newCartID() == id {
		t.Fatalf("cart id %q", id)
	}
}
