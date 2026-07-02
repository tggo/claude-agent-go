package budget

import (
	"math"
	"testing"
)

func TestTrackerBasics(t *testing.T) {
	tr := New(1.00)
	tr.Add("s1", 0.30)
	tr.Add("s2", 0.20)
	tr.Add("s1", 0.10)

	if s := tr.Spent(); math.Abs(s-0.60) > 1e-9 {
		t.Errorf("Spent = %v, want 0.60", s)
	}
	if s := tr.SessionSpent("s1"); math.Abs(s-0.40) > 1e-9 {
		t.Errorf("SessionSpent(s1) = %v, want 0.40", s)
	}
	if r := tr.Remaining(); math.Abs(r-0.40) > 1e-9 {
		t.Errorf("Remaining = %v, want 0.40", r)
	}
	if tr.Exceeded() {
		t.Error("should not be exceeded")
	}
	if !tr.CanSpend(0.40) || tr.CanSpend(0.41) {
		t.Error("CanSpend boundary wrong")
	}
}

func TestTrackerUncapped(t *testing.T) {
	tr := New(0) // unlimited
	tr.Add("", 100)
	if tr.Exceeded() {
		t.Error("uncapped should never exceed")
	}
	if !tr.CanSpend(1e9) {
		t.Error("uncapped CanSpend should be true")
	}
	if !math.IsInf(tr.Remaining(), 1) {
		t.Errorf("uncapped Remaining = %v, want +Inf", tr.Remaining())
	}
}

func TestTrackerCallbacks(t *testing.T) {
	var warns, exceeds int
	var lastExceed Snapshot
	tr := New(1.00,
		WithWarnFraction(0.8),
		OnWarn(func(Snapshot) { warns++ }),
		OnExceed(func(s Snapshot) { exceeds++; lastExceed = s }),
	)
	tr.Add("", 0.50) // 50% — no warn
	if warns != 0 {
		t.Fatalf("warn fired too early")
	}
	tr.Add("", 0.35) // 85% — warn once
	tr.Add("", 0.05) // 90% — still one warn (fires once)
	if warns != 1 {
		t.Errorf("warns = %d, want 1", warns)
	}
	tr.Add("", 0.20) // 110% — exceed
	if exceeds != 1 {
		t.Errorf("exceeds = %d, want 1", exceeds)
	}
	if lastExceed.Spent <= lastExceed.Max || lastExceed.Remaining != 0 {
		t.Errorf("exceed snapshot wrong: %+v", lastExceed)
	}
}

// callback re-entrancy must not deadlock (Add fires callbacks after unlock).
func TestTrackerCallbackReentrant(t *testing.T) {
	var tr *Tracker
	tr = New(1.00, OnExceed(func(Snapshot) { _ = tr.Spent() }))
	tr.Add("", 2.0) // fires OnExceed which reads Spent() — must not deadlock
}
