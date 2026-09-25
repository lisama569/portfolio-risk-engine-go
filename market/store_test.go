package market

import (
	"errors"
	"testing"
	"time"
)

var base = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

func logQuote(t *testing.T, action string, q Quote) {
	t.Helper()
	t.Logf("输入快照: action=%s symbol=%s price=%.4f asOf=%s seq=%d",
		action, q.Symbol, q.Price, q.AsOf.Format(time.RFC3339), q.Seq)
}

func logVerdict(t *testing.T, err error, want RejectReason) {
	t.Helper()
	got := RejectReason("ACCEPTED")
	var re *RejectError
	if errors.As(err, &re) {
		got = re.Reason
	}
	t.Logf("判定依据: 期望原因=%s 实际原因=%s", want, got)
}

func TestApplyRejectsInvalidPrice(t *testing.T) {
	s := NewStore(time.Hour)
	for _, p := range []float64{0, -1, 1e-18 * 0} {
		q := Quote{Symbol: "AAA", Price: p, AsOf: base, Seq: 1}
		logQuote(t, "apply-invalid", q)
		err := s.Apply(q)
		logVerdict(t, err, ReasonInvalidPrice)
		var re *RejectError
		if !errors.As(err, &re) || re.Reason != ReasonInvalidPrice {
			t.Fatalf("price=%v: want %s, got %v", p, ReasonInvalidPrice, err)
		}
	}
}

func TestApplyDuplicateAndOutOfOrder(t *testing.T) {
	s := NewStore(time.Hour)
	q1 := Quote{Symbol: "AAA", Price: 100, AsOf: base, Seq: 1}
	logQuote(t, "apply-first", q1)
	if err := s.Apply(q1); err != nil {
		t.Fatalf("first apply failed: %v", err)
	}

	dup := Quote{Symbol: "AAA", Price: 100, AsOf: base, Seq: 1}
	logQuote(t, "apply-duplicate", dup)
	err := s.Apply(dup)
	logVerdict(t, err, ReasonDuplicate)
	var re *RejectError
	if !errors.As(err, &re) || re.Reason != ReasonDuplicate {
		t.Fatalf("want %s, got %v", ReasonDuplicate, err)
	}

	ooo := Quote{Symbol: "AAA", Price: 99, AsOf: base.Add(-time.Minute), Seq: 0}
	logQuote(t, "apply-out-of-order", ooo)
	err = s.Apply(ooo)
	logVerdict(t, err, ReasonOutOfOrder)
	if !errors.As(err, &re) || re.Reason != ReasonOutOfOrder {
		t.Fatalf("want %s, got %v", ReasonOutOfOrder, err)
	}

	// 拒绝后状态不得漂移：价格仍为首次值
	got, gerr := s.Get("AAA", base)
	if gerr != nil || got.Price != 100 {
		t.Fatalf("state drifted after rejected updates: %+v err=%v", got, gerr)
	}
	t.Logf("判定依据: 拒绝重复/乱序后价格仍为 %.2f，未发生重复计价", got.Price)
}

func TestGetMissingAndStale(t *testing.T) {
	s := NewStore(30 * time.Minute)
	q := Quote{Symbol: "BBB", Price: 50, AsOf: base, Seq: 1}
	logQuote(t, "apply", q)
	if err := s.Apply(q); err != nil {
		t.Fatalf("apply failed: %v", err)
	}

	_, err := s.Get("NOPE", base)
	logVerdict(t, err, ReasonMissing)
	var re *RejectError
	if !errors.As(err, &re) || re.Reason != ReasonMissing {
		t.Fatalf("want %s, got %v", ReasonMissing, err)
	}

	asOf := base.Add(31 * time.Minute)
	_, err = s.Get("BBB", asOf)
	t.Logf("输入快照: symbol=BBB quoteAsOf=%s valuationAsOf=%s maxStaleness=30m",
		base.Format(time.RFC3339), asOf.Format(time.RFC3339))
	logVerdict(t, err, ReasonStale)
	if !errors.As(err, &re) || re.Reason != ReasonStale {
		t.Fatalf("want %s, got %v", ReasonStale, err)
	}

	// 未过期时应成功
	got, err := s.Get("BBB", base.Add(29*time.Minute))
	if err != nil || got.Price != 50 {
		t.Fatalf("fresh quote rejected: %+v err=%v", got, err)
	}
}
