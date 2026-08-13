package bucket

import (
	"testing"
	"time"
)

func TestBucketRefillTakeWaitRefund(t *testing.T) {
	now := time.Unix(0, 0)
	b := New(10, 5, now)

	if !b.Take(5) {
		t.Fatal("full bucket must grant its burst")
	}
	if b.Take(1) {
		t.Fatal("empty bucket granted a token")
	}
	if wait := b.Wait(1); wait != 100*time.Millisecond {
		t.Fatalf("wait = %s", wait)
	}

	b.Refill(now.Add(200 * time.Millisecond))
	if !b.Take(2) {
		t.Fatal("refill did not accumulate")
	}

	b.Refund(100)
	if !b.Full() {
		t.Fatal("refund must clamp to burst")
	}
	b.Refill(now.Add(time.Hour))
	if b.Burst() != 5 || !b.Full() {
		t.Fatal("refill must clamp to burst")
	}
}
