package refs

import (
	"testing"
)

func baseSnapshot() Snapshot {
	return Snapshot{
		Datasets: []Dataset{
			{Name: "tank", Mountpoint: "/tank", Type: "filesystem"},
			{Name: "tank/data", Mountpoint: "/tank/data", Type: "filesystem"},
			{Name: "tank/data/sub", Mountpoint: "/tank/data/sub", Type: "filesystem"},
			{Name: "tank/data2", Mountpoint: "/tank/data2", Type: "filesystem"},
			{Name: "tank/vm1", Mountpoint: "-", Type: "volume"},
			{Name: "tank/legacy", Mountpoint: "legacy", Type: "filesystem"},
		},
		SMB: SMBState{Initialized: true},
	}
}

func TestDestroyConflicts(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Snapshot)
		target    string
		recursive bool
		want      []Consumer
	}{
		{
			name:      "no consumers at all",
			target:    "tank/data",
			recursive: false,
			want:      nil,
		},
		{
			name: "usershare exact mountpoint, non-recursive",
			mutate: func(s *Snapshot) {
				s.SMB.Usershares = []Usershare{{Name: "docs", Path: "/tank/data"}}
			},
			target:    "tank/data",
			recursive: false,
			want: []Consumer{
				{Type: TypeSMBUsershare, ID: "docs", Name: "docs", Detail: "path /tank/data"},
			},
		},
		{
			name: "usershare subdirectory path",
			mutate: func(s *Snapshot) {
				s.SMB.Usershares = []Usershare{{Name: "docs", Path: "/tank/data/share"}}
			},
			target:    "tank/data",
			recursive: false,
			want: []Consumer{
				{Type: TypeSMBUsershare, ID: "docs", Name: "docs", Detail: "path /tank/data/share"},
			},
		},
		{
			name: "string coincidence: /tank/data2 must not match tank/data",
			mutate: func(s *Snapshot) {
				s.SMB.Usershares = []Usershare{
					{Name: "two", Path: "/tank/data2"},
					{Name: "inside", Path: "/tank/data/x"},
				}
			},
			target:    "tank/data",
			recursive: false,
			want: []Consumer{
				{Type: TypeSMBUsershare, ID: "inside", Name: "inside", Detail: "path /tank/data/x"},
			},
		},
		{
			name: "path owned by closer child mountpoint: destroy parent non-recursive does not block",
			mutate: func(s *Snapshot) {
				s.SMB.Usershares = []Usershare{{Name: "subshare", Path: "/tank/data/sub/x"}}
			},
			target:    "tank/data",
			recursive: false,
			want:      nil,
		},
		{
			name: "path owned by closer child mountpoint: recursive destroy blocks",
			mutate: func(s *Snapshot) {
				s.SMB.Usershares = []Usershare{{Name: "subshare", Path: "/tank/data/sub/x"}}
			},
			target:    "tank/data",
			recursive: true,
			want: []Consumer{
				{Type: TypeSMBUsershare, ID: "subshare", Name: "subshare", Detail: "path /tank/data/sub/x"},
			},
		},
		{
			name: "path owned by parent: recursive destroy of parent blocks even when child dataset exists elsewhere in path",
			mutate: func(s *Snapshot) {
				// /tank/data/other is not itself a dataset; longest owner is tank/data.
				s.SMB.Usershares = []Usershare{{Name: "p", Path: "/tank/data/other"}}
			},
			target:    "tank/data",
			recursive: true,
			want: []Consumer{
				{Type: TypeSMBUsershare, ID: "p", Name: "p", Detail: "path /tank/data/other"},
			},
		},
		{
			name: "share path outside ZFS never blocks",
			mutate: func(s *Snapshot) {
				s.SMB.Usershares = []Usershare{{Name: "etc", Path: "/srv/external"}}
			},
			target:    "tank/data",
			recursive: true,
			want:      nil,
		},
		{
			name: "SMB not initialised: SMB consumers skipped entirely",
			mutate: func(s *Snapshot) {
				s.SMB.Initialized = false
				s.SMB.Usershares = []Usershare{{Name: "docs", Path: "/tank/data"}}
				s.SMB.HomesBase = "/tank/data"
				s.SMB.TimeMachine = []TMShare{{Name: "tm", Path: "/tank/data"}}
			},
			target:    "tank/data",
			recursive: true,
			want:      nil,
		},
		{
			name: "homes base path with %U suffix",
			mutate: func(s *Snapshot) {
				s.SMB.HomesBase = "/tank/data/%U"
			},
			target:    "tank/data",
			recursive: false,
			want: []Consumer{
				{Type: TypeSMBHomes, ID: "homes", Name: "[homes]", Detail: "base path /tank/data"},
			},
		},
		{
			name: "homes under subdirectory with lowercase substitution and trailing slash",
			mutate: func(s *Snapshot) {
				s.SMB.HomesBase = "/tank/data/homes/%u/"
			},
			target:    "tank/data",
			recursive: true,
			want: []Consumer{
				{Type: TypeSMBHomes, ID: "homes", Name: "[homes]", Detail: "base path /tank/data/homes"},
			},
		},
		{
			name: "homes absent does not block",
			mutate: func(s *Snapshot) {
				s.SMB.HomesBase = ""
			},
			target:    "tank/data",
			recursive: false,
			want:      nil,
		},
		{
			name: "time machine shares",
			mutate: func(s *Snapshot) {
				s.SMB.TimeMachine = []TMShare{
					{Name: "tm-b", Path: "/tank/data/tmb"},
					{Name: "tm-a", Path: "/tank/data2/tm"}, // owned by tank/data2
				}
			},
			target:    "tank/data",
			recursive: false,
			want: []Consumer{
				{Type: TypeSMBTimeMachine, ID: "tm-b", Name: "tm-b", Detail: "path /tank/data/tmb"},
			},
		},
		{
			name: "iscsi zvol exact match",
			mutate: func(s *Snapshot) {
				s.ISCSITargets = []ISCSITarget{{IQN: "iqn.2020-01.com.example:vm1", Zvol: "tank/vm1"}}
			},
			target:    "tank/vm1",
			recursive: false,
			want: []Consumer{
				{Type: TypeISCSITarget, ID: "iqn.2020-01.com.example:vm1", Name: "iqn.2020-01.com.example:vm1", Detail: "zvol tank/vm1"},
			},
		},
		{
			name: "iscsi zvol descendant blocked only on recursive destroy",
			mutate: func(s *Snapshot) {
				s.ISCSITargets = []ISCSITarget{{IQN: "iqn.2020-01.com.example:child", Zvol: "tank/data/vol"}}
			},
			target:    "tank/data",
			recursive: false,
			want:      nil,
		},
		{
			name: "iscsi zvol descendant blocked on recursive destroy",
			mutate: func(s *Snapshot) {
				s.ISCSITargets = []ISCSITarget{{IQN: "iqn.2020-01.com.example:child", Zvol: "tank/data/vol"}}
			},
			target:    "tank/data",
			recursive: true,
			want: []Consumer{
				{Type: TypeISCSITarget, ID: "iqn.2020-01.com.example:child", Name: "iqn.2020-01.com.example:child", Detail: "zvol tank/data/vol"},
			},
		},
		{
			name: "replication local source and target both block",
			mutate: func(s *Snapshot) {
				s.ReplTasks = []ReplTask{{ID: "id1", Name: "nightly", Source: "tank/data", Target: "tank/backup/data", Remote: ""}}
			},
			target:    "tank/data",
			recursive: false,
			want: []Consumer{
				{Type: TypeReplicationSource, ID: "id1", Name: "nightly", Detail: "source tank/data"},
			},
		},
		{
			name: "replication local target blocks when destroying destination subtree",
			mutate: func(s *Snapshot) {
				s.Datasets = append(s.Datasets, Dataset{Name: "tank/backup", Mountpoint: "/tank/backup", Type: "filesystem"})
				s.ReplTasks = []ReplTask{{ID: "id1", Name: "nightly", Source: "tank/data", Target: "tank/backup/data", Remote: ""}}
			},
			target:    "tank/backup",
			recursive: true,
			want: []Consumer{
				{Type: TypeReplicationLocalTarget, ID: "id1", Name: "nightly", Detail: "local target tank/backup/data"},
			},
		},
		{
			name: "remote replication target does not block; source still does",
			mutate: func(s *Snapshot) {
				s.Datasets = append(s.Datasets, Dataset{Name: "tank/backup", Mountpoint: "/tank/backup", Type: "filesystem"})
				s.ReplTasks = []ReplTask{{ID: "id1", Name: "remote", Source: "tank/data", Target: "tank/backup/data", Remote: "backup@host"}}
			},
			target:    "tank/backup",
			recursive: true,
			want:      nil,
		},
		{
			name: "disabled-independent: replication source descendant blocks recursively",
			mutate: func(s *Snapshot) {
				s.ReplTasks = []ReplTask{{ID: "id9", Name: "deep", Source: "tank/data/sub", Target: "tank/else", Remote: ""}}
			},
			target:    "tank/data",
			recursive: true,
			want: []Consumer{
				{Type: TypeReplicationSource, ID: "id9", Name: "deep", Detail: "source tank/data/sub"},
			},
		},
		{
			name: "replication source is ancestor of destroyed dataset: not a conflict",
			mutate: func(s *Snapshot) {
				s.ReplTasks = []ReplTask{{ID: "id9", Name: "deep", Source: "tank", Target: "tank/else", Remote: ""}}
			},
			target:    "tank/data/sub",
			recursive: true,
			want:      nil,
		},
		{
			name: "task name falls back to id in consumer display",
			mutate: func(s *Snapshot) {
				s.ReplTasks = []ReplTask{{ID: "id42", Source: "tank/data", Target: "tank/x", Remote: "host@remote"}}
			},
			target:    "tank/data",
			recursive: false,
			want: []Consumer{
				{Type: TypeReplicationSource, ID: "id42", Name: "id42", Detail: "source tank/data"},
			},
		},
		{
			name: "legacy-mount dataset path references do not attribute",
			mutate: func(s *Snapshot) {
				s.SMB.Usershares = []Usershare{{Name: "leg", Path: "/tank/legacy"}}
			},
			target:    "tank/legacy",
			recursive: true,
			want:      nil,
		},
		{
			name: "full mix is sorted by (type,id)",
			mutate: func(s *Snapshot) {
				s.SMB.Usershares = []Usershare{
					{Name: "zshare", Path: "/tank/data"},
					{Name: "ashare", Path: "/tank/data"},
				}
				s.SMB.HomesBase = "/tank/data"
				s.SMB.TimeMachine = []TMShare{{Name: "tm", Path: "/tank/data"}}
				s.ISCSITargets = []ISCSITarget{{IQN: "iqn.2020-01.com.example:vm1", Zvol: "tank/data/v"}}
				s.ReplTasks = []ReplTask{
					{ID: "t2", Name: "two", Source: "tank/data", Target: "tank/o2", Remote: ""},
					{ID: "t1", Name: "one", Source: "tank/data", Target: "tank/o1", Remote: ""},
				}
			},
			target:    "tank/data",
			recursive: true,
			want: []Consumer{
				{Type: TypeISCSITarget, ID: "iqn.2020-01.com.example:vm1", Name: "iqn.2020-01.com.example:vm1", Detail: "zvol tank/data/v"},
				{Type: TypeReplicationSource, ID: "t1", Name: "one", Detail: "source tank/data"},
				{Type: TypeReplicationSource, ID: "t2", Name: "two", Detail: "source tank/data"},
				{Type: TypeSMBHomes, ID: "homes", Name: "[homes]", Detail: "base path /tank/data"},
				{Type: TypeSMBTimeMachine, ID: "tm", Name: "tm", Detail: "path /tank/data"},
				{Type: TypeSMBUsershare, ID: "ashare", Name: "ashare", Detail: "path /tank/data"},
				{Type: TypeSMBUsershare, ID: "zshare", Name: "zshare", Detail: "path /tank/data"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := baseSnapshot()
			if tc.mutate != nil {
				tc.mutate(&s)
			}
			got := s.DestroyConflicts(tc.target, tc.recursive)
			if len(got) != len(tc.want) {
				t.Fatalf("conflict count = %d, want %d: %#v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("conflict[%d] = %#v, want %#v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestHomesBasePath(t *testing.T) {
	tests := map[string]string{
		"/tank/data/%U":   "/tank/data",
		"/tank/data/%u":   "/tank/data",
		"/tank/data/%U/":  "/tank/data",
		"/tank/data/homes": "/tank/data/homes",
		"/tank/data/":     "/tank/data",
		"  /tank/x/%U ":   "/tank/x",
	}
	for in, want := range tests {
		if got := homesBasePath(in); got != want {
			t.Errorf("homesBasePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGuardSerialises(t *testing.T) {
	// Sanity: Guard is a plain mutex usable as the admission boundary.
	g := NewGuard()
	g.Lock()
	got := make(chan struct{})
	go func() {
		g.Lock()
		defer g.Unlock()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("guard did not serialise")
	default:
	}
	g.Unlock()
}
