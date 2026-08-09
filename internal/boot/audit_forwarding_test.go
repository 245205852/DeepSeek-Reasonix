package boot

import (
	"reflect"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/notify"
	"reasonix/internal/stats"
	"reasonix/internal/trajectory"
)

// Optional sink capabilities are forwarded by hand at every wrapper in the
// chain, so a new one silently loses its data at whichever wrapper forgot it.
// That is exactly how the delegation audit reached no metrics sink at all until
// control's goal-usage tee was taught to forward it, while three layers of unit
// tests passed. Every wrapper that forwards one audit must forward them all.
func TestAuditCapabilitiesAreForwardedByEveryWrapper(t *testing.T) {
	reference := reflect.TypeFor[event.CompletionReportAuditSink]()
	required := map[string]reflect.Type{
		"DelegationAuditSink": reflect.TypeFor[event.DelegationAuditSink](),
		"ReadinessAuditSink":  reflect.TypeFor[event.ReadinessAuditSink](),
	}
	wrappers := []event.Sink{
		event.Sync(event.Discard),
		event.Coalesce(event.Discard, time.Millisecond),
		control.NewGoalUsageTee(event.Discard),
		notify.NewSink(event.Discard, nil, config.NotificationsConfig{}),
		&trajectory.Recorder{},
		&stats.Recorder{},
	}
	checked := 0
	for _, w := range wrappers {
		wt := reflect.TypeOf(w)
		if !wt.Implements(reference) {
			continue
		}
		checked++
		for name, want := range required {
			if !wt.Implements(want) {
				t.Errorf("%s forwards CompletionReportAudit but not %s: the audit dies at this wrapper", wt, name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no wrapper was checked; the reference capability must have moved")
	}
}
