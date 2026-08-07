package main

import "testing"

func TestRuntimeDoctorEmptyApp(t *testing.T) {
	var a *App
	report := a.RuntimeDoctor()
	if report.Text == "" {
		t.Fatal("expected doctor text")
	}
	// Nil app still returns process-wide metrics/recoverability.
	if !report.AllowResume {
		t.Fatal("empty process should allow resume")
	}
}
