package api

import (
	"fmt"
	"runtime"

	"dumpstore/internal/iscsi"
	"dumpstore/internal/refs"
	"dumpstore/internal/smb"
	"dumpstore/internal/zfs"
)

// gatherRefSnapshot builds a consistent read model of every dumpstore-managed
// consumer that can reference a ZFS dataset: SMB usershares, the [homes] base
// path, Time Machine shares, iSCSI zvol-backed targets, and replication
// tasks (source always; target only when the task is local).
//
// MUST be called with h.guard held so that no consumer create/modify/remove
// can interleave with the read. Any backend or configuration read failure is
// returned to the caller: the destroy path treats that as fail-closed and
// never invokes the destroy playbook. Backend absence (SMB not initialised,
// no iSCSI backend installed) legitimately yields empty sections.
func (h *Handler) gatherRefSnapshot() (refs.Snapshot, error) {
	var snap refs.Snapshot

	datasets, err := zfs.ListDatasets()
	if err != nil {
		return snap, fmt.Errorf("list datasets for reference check: %w", err)
	}
	for _, d := range datasets {
		snap.Datasets = append(snap.Datasets, refs.Dataset{
			Name:       d.Name,
			Mountpoint: d.Mountpoint,
			Type:       d.Type,
		})
	}

	goos := runtime.GOOS
	if smb.IsInitialized(goos) {
		snap.SMB.Initialized = true

		cfg, err := smb.ParseSMBConfig(goos)
		if err != nil {
			return snap, fmt.Errorf("read smb config for reference check: %w", err)
		}
		usershares, err := smb.ListUsershares(goos)
		if err != nil {
			return snap, fmt.Errorf("list smb usershares for reference check: %w", err)
		}
		for _, u := range usershares {
			snap.SMB.Usershares = append(snap.SMB.Usershares, refs.Usershare{
				Name: u.Name,
				Path: u.Path,
			})
		}
		if cfg.Homes != nil {
			snap.SMB.HomesBase = cfg.Homes.Path
		}
		for _, tm := range cfg.TimeMachine {
			snap.SMB.TimeMachine = append(snap.SMB.TimeMachine, refs.TMShare{
				Name: tm.Name,
				Path: tm.Path,
			})
		}
	}

	targets, err := iscsi.ListTargets()
	if err != nil {
		return snap, fmt.Errorf("list iscsi targets for reference check: %w", err)
	}
	for _, t := range targets {
		snap.ISCSITargets = append(snap.ISCSITargets, refs.ISCSITarget{
			IQN:  t.IQN,
			Zvol: t.ZvolName,
		})
	}

	// h.repl is always wired by NewHandler; guard anyway for nil-safe tests.
	if h.repl != nil {
		for _, t := range h.repl.Store().List() {
			snap.ReplTasks = append(snap.ReplTasks, refs.ReplTask{
				ID:     t.ID,
				Name:   t.Name,
				Source: t.Source,
				Target: t.Target,
				Remote: t.Remote,
			})
		}
	}

	return snap, nil
}
