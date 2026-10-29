package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const valid = `
[remote.nas]
host = "nas.local"
port = 2222
user = "user"
shell_path_map = { "/photo" = "/volume1/photo" }

[[camera]]
id = "GR3x"
match = [{ make = "RICOH IMAGING COMPANY, LTD.", model = "RICOH GR IIIx" }]
remote = "nas"
dest = "/photo/GR3x/"

[[camera]]
id = "Go3"
volume_label = "Insta360GO3"
remote = "nas"
dest = "/home/backups/Insta360Go3"
`

func TestParseValid(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Verify != VerifyHash {
		t.Errorf("verify default = %q", cfg.Defaults.Verify)
	}
	if cfg.Defaults.Jobs != DefaultJobs {
		t.Errorf("jobs default = %d", cfg.Defaults.Jobs)
	}
	if cfg.Defaults.OnConflict != OnConflictAbort {
		t.Errorf("on_conflict default = %q", cfg.Defaults.OnConflict)
	}
	if got := cfg.Cameras[0].Dest; got != "/photo/GR3x" {
		t.Errorf("dest not cleaned: %q", got)
	}
	if cfg.Remotes["nas"].Port != 2222 {
		t.Errorf("port = %d", cfg.Remotes["nas"].Port)
	}
	if _, ok := cfg.Camera("Go3"); !ok {
		t.Error("Camera(Go3) not found")
	}
}

func TestDefaultPort(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(valid, "port = 2222\n", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remotes["nas"].Port != 22 {
		t.Errorf("port = %d, want 22", cfg.Remotes["nas"].Port)
	}
}

// Table-driven test: each case breaks the valid config in one way and
// checks that the error message mentions the problem.
func TestParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		from    string
		to      string
		wantErr string
	}{
		{"unknown key (typo)", `port = 2222`, "port = 2222\nprot = 1", "prot"},
		{"bad verify", `[remote.nas]`, "[defaults]\nverify = \"md5\"\n[remote.nas]", "defaults.verify"},
		{"too many jobs", `[remote.nas]`, "[defaults]\njobs = 9\n[remote.nas]", "defaults.jobs: must be between 1 and 8"},
		{"negative jobs", `[remote.nas]`, "[defaults]\njobs = -1\n[remote.nas]", "defaults.jobs"},
		{"zero jobs", `[remote.nas]`, "[defaults]\njobs = 0\n[remote.nas]", "defaults.jobs: must be between 1 and 8, got 0"},
		{"bad on_conflict", `[remote.nas]`, "[defaults]\non_conflict = \"overwrite\"\n[remote.nas]", `defaults.on_conflict: must be "abort", "skip" or "rename", got "overwrite"`},
		{"relative dest", `dest = "/photo/GR3x/"`, `dest = "photo/GR3x"`, "absolute SFTP path"},
		{"undefined remote", `remote = "nas"`, `remote = "nas2"`, `remote "nas2" is not defined`},
		{"duplicate id", `id = "Go3"`, `id = "GR3x"`, "duplicate id"},
		{"empty model", `model = "RICOH GR IIIx"`, `model = ""`, "make and model are both required"},
		{"relative shell map", `"/photo" = "/volume1/photo"`, `"photo" = "/volume1/photo"`, "shell_path_map"},
		{"duplicate match across cameras", `volume_label = "Insta360GO3"`, "volume_label = \"Insta360GO3\"\nmatch = [{ make = \" RICOH IMAGING COMPANY, LTD.\", model = \"RICOH GR IIIx\" }]", `is already used by camera "GR3x"`},
		{"missing user", `user = "user"`, ``, "user: required"},
		{"file mode without 0o", `port = 2222`, "port = 2222\nfile_mode = 644", "remote.nas.file_mode: 644 is not a permission from 0o000 to 0o777; write it in octal, like 0o600"},
		{"file mode with special bits", `port = 2222`, "port = 2222\nfile_mode = 0o4644", "remote.nas.file_mode"},
		{"zero file mode", `port = 2222`, "port = 2222\nfile_mode = 0", "remote.nas.file_mode: 0o000 must give the owner read and write"},
		{"read-only file mode", `port = 2222`, "port = 2222\nfile_mode = 0o444", "remote.nas.file_mode: 0o444 must give the owner read and write (0o600)"},
		{"dir mode without search", `port = 2222`, "port = 2222\ndir_mode = 0o644", "remote.nas.dir_mode: 0o644 must give the owner read, write and search (0o700)"},
		{"string file mode", `port = 2222`, "port = 2222\nfile_mode = \"0644\"", "FileMode"}, // go-toml names the Go field
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(valid, tc.from, tc.to, 1)
			if src == valid {
				t.Fatalf("replacement %q not applied", tc.from)
			}
			_, err := Parse([]byte(src))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestModes(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.Remotes["nas"]; r.FileMode != nil || r.DirMode != nil {
		t.Errorf("modes set without being configured: %v %v", r.FileMode, r.DirMode)
	}

	cfg, err = Parse([]byte(strings.Replace(valid, "port = 2222\n", "port = 2222\nfile_mode = 0o640\ndir_mode = 0o750\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Remotes["nas"]
	if r.FileMode == nil || *r.FileMode != 0o640 {
		t.Errorf("file_mode = %v, want 0o640", r.FileMode)
	}
	if r.DirMode == nil || *r.DirMode != 0o750 {
		t.Errorf("dir_mode = %v, want 0o750", r.DirMode)
	}
}

func TestResolvePath(t *testing.T) {
	t.Setenv(EnvConfig, "/from/env.toml")
	if got, _ := ResolvePath("/from/flag.toml"); got != "/from/flag.toml" {
		t.Errorf("flag should win, got %q", got)
	}
	if got, _ := ResolvePath(""); got != "/from/env.toml" {
		t.Errorf("env should be used, got %q", got)
	}
	if _, from, _ := ResolvePathFrom("/from/flag.toml"); from != FromFlag {
		t.Errorf("from = %q, want %q", from, FromFlag)
	}
	if _, from, _ := ResolvePathFrom(""); from != FromEnv {
		t.Errorf("from = %q, want %q", from, FromEnv)
	}
	t.Setenv(EnvConfig, "")
	if _, from, _ := ResolvePathFrom(""); from != FromDefault {
		t.Errorf("from = %q, want %q", from, FromDefault)
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	got, err := ExpandHome("~/.ssh/id_ed25519")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".ssh", "id_ed25519"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got, _ := ExpandHome("/abs"); got != "/abs" {
		t.Errorf("absolute path changed: %q", got)
	}
}

// The shipped example must always be a valid config.
func TestExampleConfig(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "config.example.toml")); err != nil {
		t.Fatal(err)
	}
}
