package extension

import "testing"

func TestMessageSendGuardDedup(t *testing.T) {
	g := NewMessageSendGuard()
	if !g.TryRecord(1, "m1") {
		t.Fatal("first should succeed")
	}
	if g.TryRecord(1, "m1") {
		t.Fatal("duplicate should fail")
	}
	if !g.TryRecord(1, "m2") {
		t.Fatal("different id ok")
	}
	if !g.TryRecord(2, "m1") {
		t.Fatal("different gen ok")
	}
}

func TestRecordMessageSentOnce(t *testing.T) {
	if !RecordMessageSentOnce(77, "once-a", "test") {
		t.Fatal("first")
	}
	if RecordMessageSentOnce(77, "once-a", "test") {
		t.Fatal("second must be rejected")
	}
}
