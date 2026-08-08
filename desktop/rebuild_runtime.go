package main

import (
	"reasonix/internal/boot"
	"reasonix/internal/control"
)

func rebuildTabRuntime(a *App, tab *WorkspaceTab, old *control.Controller, opts boot.Options) (*boot.BuildResult, error) {
	var res *boot.BuildResult
	var err error
	if tab != nil && tab.lastBuildResult != nil {
		res, err = boot.RebuildFrom(a.bootContext(), tab.lastBuildResult, opts)
	} else {
		res, err = boot.Rebuild(a.bootContext(), old, opts)
	}
	if err != nil {
		return nil, err
	}
	if tab != nil {
		tab.lastBuildResult = res
	}
	return res, nil
}

// RuntimeDoctorReport is the desktop/Wails view of extension runtime diagnostics.
type RuntimeDoctorReport struct {
	Text                  string `json:"text"`
	PublishedGen          uint64 `json:"publishedGeneration"`
	AllowResume           bool   `json:"allowResume"`
	CleanRollback         bool   `json:"cleanRollback"`
	HasIrreversible       bool   `json:"hasIrreversible"`
	NoOpRebuilds          uint64 `json:"noOpRebuilds"`
	FullRebuilds          uint64 `json:"fullRebuilds"`
	SubgraphRebuilds      uint64 `json:"subgraphRebuilds"`
	StaleDrops            uint64 `json:"staleDrops"`
	AdmissionRejected     uint64 `json:"admissionRejected"`
	RuntimeOwnerFallbacks uint64 `json:"runtimeOwnerFallbacks"`
}

// RuntimeDoctor returns process-wide + active-tab extension runtime diagnostics
// for the settings/status panel (mirrors `reasonix doctor runtime`).
func (a *App) RuntimeDoctor() RuntimeDoctorReport {
	var res *boot.BuildResult
	if a != nil {
		// Prefer the active tab's last build when available.
		if tab := a.activeTab(); tab != nil {
			res = tab.lastBuildResult
		}
	}
	report := boot.CollectRuntimeDoctor(res)
	return RuntimeDoctorReport{
		Text:                  boot.RenderRuntimeDoctorText(report),
		PublishedGen:          report.PublishedGen,
		AllowResume:           report.Resume.AllowResume,
		CleanRollback:         report.Resume.CleanRollback,
		HasIrreversible:       report.Resume.HasIrreversible,
		NoOpRebuilds:          report.Metrics.NoOpRebuilds,
		FullRebuilds:          report.Metrics.FullRebuilds,
		SubgraphRebuilds:      report.Metrics.SubgraphRebuilds,
		StaleDrops:            report.Metrics.StaleDrops,
		AdmissionRejected:     report.Metrics.AdmissionRejected,
		RuntimeOwnerFallbacks: report.RuntimeOwnerFallbacks,
	}
}
