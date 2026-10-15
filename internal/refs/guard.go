// Package refs implements the pre-destroy reference guard for ZFS datasets.
//
// Before a dataset destroy playbook runs, the API layer gathers a consistent
// Snapshot of every dumpstore-managed consumer that can reference storage
// objects (SMB usershares, the [homes] base path, Time Machine shares, iSCSI
// zvol-backed targets, and replication tasks) and asks DestroyConflicts which
// consumers would be left dangling. The package performs no I/O itself: the
// API layer adapts the real backends (zfs/smb/iscsi/replication) into the
// minimal data types below, which keeps the matching logic exhaustively unit
// testable and makes "fail closed" purely an adapter concern.
package refs

import (
	"sort"
	"strings"
	"sync"
)

// Stable consumer type tags. They are part of the HTTP 409 conflict response
// contract and must not be renamed.
const (
	TypeSMBUsershare          = "smb_usershare"
	TypeSMBHomes              = "smb_homes"
	TypeSMBTimeMachine        = "smb_timemachine"
	TypeISCSITarget           = "iscsi_target"
	TypeReplicationSource     = "replication_source"
	TypeReplicationLocalTarget = "replication_local_target"
)

// Dataset is the minimal ZFS dataset metadata the guard needs.
type Dataset struct {
	Name       string
	Mountpoint string // as reported by `zfs list`; "-" for volumes, "legacy"/"none" possible
	Type       string // "filesystem" or "volume"
}

// Usershare is a dumpstore-managed Samba usershare (name = share name).
type Usershare struct {
	Name string
	Path string
}

// TMShare is one Time Machine share section parsed from smb.conf.
type TMShare struct {
	Name string
	Path string
}

// SMBState is the SMB subset of the guard snapshot. When Initialized is false
// (no dumpstore-managed smb.conf) the remaining fields must be ignored: no
// dumpstore-managed SMB consumers can exist.
type SMBState struct {
	Initialized bool
	HomesBase   string      // [homes] path with the %U user suffix stripped; "" when absent
	TimeMachine []TMShare
	Usershares  []Usershare
}

// ISCSITarget is the minimal iSCSI target identity.
type ISCSITarget struct {
	IQN  string // locatable handle used by DELETE /api/iscsi-targets
	Zvol string // backing ZFS volume, e.g. "tank/vm1"
}

// ReplTask is the minimal replication task identity. Remote == "" means the
// target is a local dataset.
type ReplTask struct {
	ID     string
	Name   string
	Source string
	Target string
	Remote string
}

// Snapshot is a consistent read model gathered while Guard is held.
type Snapshot struct {
	Datasets     []Dataset
	SMB          SMBState
	ISCSITargets []ISCSITarget
	ReplTasks    []ReplTask
}

// Consumer describes one dumpstore-managed reference that blocks a destroy.
type Consumer struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// Guard is the shared admission boundary between dataset destroy and the
// create/modify/remove operations of every consumer covered by Snapshot.
// Destroy holds the lock across gather → check → playbook, and consumer
// mutations hold it across their commit (playbook/store write), so no
// reference can appear after the destroy check passes.
type Guard struct {
	mu sync.Mutex
}

// NewGuard constructs a Guard.
func NewGuard() *Guard { return &Guard{} }

// Lock acquires the admission lock.
func (g *Guard) Lock() { g.mu.Lock() }

// Unlock releases the admission lock.
func (g *Guard) Unlock() { g.mu.Unlock() }

// destroySet returns the set of dataset names the destroy would remove.
// Non-recursive destroys remove only the target; recursive ones remove the
// target and every descendant dataset present in the snapshot (component
// boundary prefix — "tank/da" must not match "tank/data"). Volumes are
// included: `zfs list` reports them alongside filesystems.
func destroySet(target string, recursive bool, datasets []Dataset) map[string]bool {
	set := map[string]bool{target: true}
	if !recursive {
		return set
	}
	prefix := target + "/"
	for _, d := range datasets {
		if strings.HasPrefix(d.Name, prefix) {
			set[d.Name] = true
		}
	}
	return set
}

// realMountpoint reports whether mp is a filesystem location a share path can
// live under. Volumes print "-"; unmountable datasets print "legacy"/"none".
func realMountpoint(mp string) bool {
	return strings.HasPrefix(mp, "/")
}

// mountOwner maps a filesystem path to the dataset whose mountpoint owns it:
// the longest mountpoint that equals the path or is a proper path-segment
// prefix of it. Attributing by the closest mountpoint (rather than by string
// coincidence) means a share at /tank/data/sub/x belongs to a child dataset
// tank/data/sub when that dataset has its own mountpoint.
//
// Returns "" when no dataset owns the path (outside ZFS or under a
// legacy/none mountpoint).
type mountEntry struct {
	mp   string
	name string
}

func (s Snapshot) mountIndex() []mountEntry {
	var idx []mountEntry
	for _, d := range s.Datasets {
		if d.Type == "volume" {
			continue
		}
		if !realMountpoint(d.Mountpoint) {
			continue
		}
		idx = append(idx, mountEntry{mp: strings.TrimSuffix(d.Mountpoint, "/"), name: d.Name})
	}
	sort.Slice(idx, func(i, j int) bool { return len(idx[i].mp) > len(idx[j].mp) })
	return idx
}

func (s Snapshot) ownerOf(path string) string {
	path = strings.TrimSuffix(path, "/")
	if path == "" || !strings.HasPrefix(path, "/") {
		return ""
	}
	for _, e := range s.mountIndex() {
		if path == e.mp || strings.HasPrefix(path, e.mp+"/") {
			return e.name
		}
	}
	return ""
}

// pathBlocks reports whether a configured filesystem path is located inside
// any dataset in the destroy set.
func (s Snapshot) pathBlocks(path string, set map[string]bool) (string, bool) {
	owner := s.ownerOf(path)
	if owner == "" {
		return "", false
	}
	return owner, set[owner]
}

// nameDestroyed reports whether a referenced ZFS dataset/volume name is
// removed by the destroy. Exact set membership always counts; descendant
// prefixes only count for recursive destroys (a non-recursive destroy leaves
// all children in place and fails at the ZFS layer with "has children").
// The prefix check also catches names configured in a consumer backend but
// absent from the zfs list snapshot (stale backend config).
func nameDestroyed(name, target string, recursive bool, set map[string]bool) bool {
	if name == "" {
		return false
	}
	if set[name] {
		return true
	}
	return recursive && strings.HasPrefix(name, target+"/")
}

// homesBasePath strips the per-user substitution suffix (and trailing slash)
// from a [homes] path to obtain the base directory that must survive.
func homesBasePath(path string) string {
	p := strings.TrimSpace(path)
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimSuffix(p, "/%U")
	p = strings.TrimSuffix(p, "/%u")
	return strings.TrimSuffix(p, "/")
}

// DestroyConflicts returns every dumpstore-managed consumer that references
// the target dataset (non-recursive) or the target plus its descendants
// (recursive). The returned slice is sorted by (type, id) for a stable
// response. A nil/empty slice means the destroy may proceed.
func (s Snapshot) DestroyConflicts(target string, recursive bool) []Consumer {
	set := destroySet(target, recursive, s.Datasets)

	var out []Consumer

	// 1. SMB usershares — path attributed by actual mountpoint ownership.
	if s.SMB.Initialized {
		for _, sh := range s.SMB.Usershares {
			if _, blocked := s.pathBlocks(sh.Path, set); blocked {
				out = append(out, Consumer{
					Type:   TypeSMBUsershare,
					ID:     sh.Name,
					Name:   sh.Name,
					Detail: "path " + sh.Path,
				})
			}
		}

		// 2. [homes] base path.
		if base := homesBasePath(s.SMB.HomesBase); base != "" {
			if _, blocked := s.pathBlocks(base, set); blocked {
				out = append(out, Consumer{
					Type:   TypeSMBHomes,
					ID:     "homes",
					Name:   "[homes]",
					Detail: "base path " + base,
				})
			}
		}

		// 3. Time Machine shares.
		for _, tm := range s.SMB.TimeMachine {
			if _, blocked := s.pathBlocks(tm.Path, set); blocked {
				out = append(out, Consumer{
					Type:   TypeSMBTimeMachine,
					ID:     tm.Name,
					Name:   tm.Name,
					Detail: "path " + tm.Path,
				})
			}
		}
	}

	// 4. iSCSI targets backed by a zvol in the destroy subtree.
	for _, t := range s.ISCSITargets {
		if nameDestroyed(t.Zvol, target, recursive, set) {
			out = append(out, Consumer{
				Type:   TypeISCSITarget,
				ID:     t.IQN,
				Name:   t.IQN,
				Detail: "zvol " + t.Zvol,
			})
		}
	}

	// 5/6. Replication tasks: source always; target only for local tasks
	// (Remote == ""). A task whose source is an ANCESTOR of the target stays
	// valid (a recursive send simply covers one fewer child) and is not
	// reported. Disabled tasks are reported too — the stored reference would
	// still dangle.
	for _, t := range s.ReplTasks {
		if nameDestroyed(t.Source, target, recursive, set) {
			name := t.Name
			if name == "" {
				name = t.ID
			}
			out = append(out, Consumer{
				Type:   TypeReplicationSource,
				ID:     t.ID,
				Name:   name,
				Detail: "source " + t.Source,
			})
		}
		if t.Remote == "" && nameDestroyed(t.Target, target, recursive, set) {
			name := t.Name
			if name == "" {
				name = t.ID
			}
			out = append(out, Consumer{
				Type:   TypeReplicationLocalTarget,
				ID:     t.ID,
				Name:   name,
				Detail: "local target " + t.Target,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].ID < out[j].ID
	})
	return out
}
