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
		{"relative dest", `dest = "/photo/GR3x/"`, `dest = "photo/GR3x"`, "absolute SFTP path"},
		{"undefined remote", `remote = "nas"`, `remote = "nas2"`, `remote "nas2" is not defined`},
		{"duplicate id", `id = "Go3"`, `id = "GR3x"`, "duplicate id"},
		{"empty model", `model = "RICOH GR IIIx"`, `model = ""`, "make and model are both required"},
		{"relative shell map", `"/photo" = "/volume1/photo"`, `"photo" = "/volume1/photo"`, "shell_path_map"},
		{"duplicate match across cameras", `volume_label = "Insta360GO3"`, "volume_label = \"Insta360GO3\"\nmatch = [{ make = \" RICOH IMAGING COMPANY, LTD.\", model = \"RICOH GR IIIx\" }]", `is already used by camera "GR3x"`},
		{"missing user", `user = "user"`, ``, "user: required"},
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

func TestResolvePath(t *testing.T) {
	t.Setenv(EnvConfig, "/from/env.toml")
	if got, _ := ResolvePath("/from/flag.toml"); got != "/from/flag.toml" {
		t.Errorf("flag should win, got %q", got)
	}
	if got, _ := ResolvePath(""); got != "/from/env.toml" {
		t.Errorf("env should be used, got %q", got)
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
