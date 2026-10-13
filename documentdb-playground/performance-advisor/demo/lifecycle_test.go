// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package demo

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSessionMCPConfigurationKeepsOnlyTelemetryToolsLoaded(t *testing.T) {
	source, err := os.ReadFile("../demo-mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Servers map[string]struct {
			DeferTools string   `json:"deferTools"`
			Tools      []string `json:"tools"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(source, &config); err != nil {
		t.Fatal(err)
	}
	server, ok := config.Servers["live-telemetry"]
	if !ok || len(config.Servers) != 1 || server.DeferTools != "never" {
		t.Fatal("the session must eagerly load only the live-telemetry server")
	}
	if !slices.Equal(server.Tools, []string{
		"get_capture_status", "get_metric_window", "get_recent_traces", "get_trace",
	}) {
		t.Fatalf("unexpected observer tools: %v", server.Tools)
	}
	launcher, err := os.ReadFile("../demo.sh")
	if err != nil {
		t.Fatal(err)
	}
	var qualified []string
	for _, name := range server.Tools {
		qualified = append(qualified, "mcp:live-telemetry-"+name)
	}
	if !slices.Contains(strings.Fields(string(launcher)), "--available-tools="+strings.Join(qualified, ",")) {
		t.Fatal("the observer allowlist must use source-qualified identities for its four MCP tools")
	}
}

func TestInheritedMCPServersAreIsolatedWithoutParsingNamesInTheShell(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the lifecycle intentionally refuses root execution")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is supplied by the repository devcontainer")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		input string
		want  string
		valid bool
	}{
		{"empty", `{"mcpServers":{}}`, "", true},
		{"only-demo", `{"mcpServers":{"live-telemetry":{}}}`, "", true},
		{"inherited", `{"mcpServers":{"other-server":{},"live-telemetry":{},"a server\nwith spaces":{}}}`,
			"a server\nwith spaces\x00other-server\x00", true},
		{"invalid-name", `{"mcpServers":{"bad\u0000name":{}}}`, "", false},
		{"missing-map", `{}`, "", false},
		{"wrong-shape", `{"mcpServers":[]}`, "", false},
		{"empty-input", "", "", false},
		{"multiple-values", `{"mcpServers":{}} {"mcpServers":{}}`, "", false},
		{"malformed", `{"mcpServers":`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("bash", "../demo.sh", "_mcp_names")
			command.Env = append(os.Environ(), "WORKSPACE_ROOT="+root)
			command.Stdin = strings.NewReader(test.input)
			var output, stderr bytes.Buffer
			command.Stdout, command.Stderr = &output, &stderr
			err := command.Run()
			if (err == nil) != test.valid || (test.valid && output.String() != test.want) {
				t.Fatalf("unexpected MCP inventory result: %q, %v, %s", output.String(), err, stderr.String())
			}
		})
	}
}

func TestLifecycleOnlyDeletesItsRecordedLocalRuntime(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the lifecycle intentionally refuses root execution")
	}
	source, err := os.ReadFile("../demo.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"owned", "missing", "already-removed", "partial-setup", "foreign-marker", "foreign-name",
		"changed-daemon", "replaced-container", "replaced-network", "replaced-volume",
		"changed-label", "missing-record", "renamed-container", "deletion-error", "symlink",
		"daemon-unavailable", "daemon-empty",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			scriptDir := filepath.Join(root, "documentdb-playground", "performance-advisor")
			bin := filepath.Join(root, "bin")
			state := filepath.Join(scriptDir, ".demo")
			for _, dir := range []string{scriptDir, bin} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, contents string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(contents), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(scriptDir, "demo.sh"), string(source))
			write(filepath.Join(bin, "git"), "#!/bin/bash\nprintf '%s\\n' \"$DEMO_FIXTURE_ROOT\"\n")
			write(filepath.Join(bin, "docker"), `#!/bin/bash
printf 'docker %s\n' "$*" >>"$DEMO_FIXTURE_ROOT/calls"
case "$1" in
  info)
    [[ "$DEMO_FIXTURE_CASE" != daemon-unavailable ]] || exit 90
    [[ "$DEMO_FIXTURE_CASE" == daemon-empty ]] || printf '%s\n' "$DEMO_FIXTURE_DAEMON"
    exit 0 ;;
  container)
    case "$2" in
      ls)
        [[ "$DEMO_FIXTURE_CASE" != already-removed && "$DEMO_FIXTURE_CASE" != partial-setup ]] || exit 0
        if [[ "$*" == *"name=^/telemetry-demo-aaaaaaaaaaaaaaaa-database$"* ]]; then
          [[ "$DEMO_FIXTURE_CASE" != renamed-container ]] || exit 0
          printf '%s\n' "$DEMO_FIXTURE_DATABASE"
        elif [[ "$*" == *"name=^/telemetry-demo-aaaaaaaaaaaaaaaa-capture$"* ]]; then
          printf '%s\n' "$DEMO_FIXTURE_CAPTURE"
        elif [[ "$*" == *"id="* && "$DEMO_FIXTURE_CASE" == renamed-container ]]; then
          printf '%s\n' "$DEMO_FIXTURE_DATABASE"
        fi ;;
      inspect) printf 'telemetry-demo-aaaaaaaaaaaaaaaa\n' ;;
      rm) printf 'container %s\n' "${@: -1}" >>"$DEMO_FIXTURE_ROOT/deleted" ;;
      *) exit 90 ;;
    esac ;;
  network)
    case "$2" in
      ls) [[ "$DEMO_FIXTURE_CASE" == already-removed ]] || printf '%s\n' "$DEMO_FIXTURE_NETWORK" ;;
      inspect)
        if [[ "$DEMO_FIXTURE_CASE" == changed-label ]]; then printf 'foreign-owner\n'
        else printf 'telemetry-demo-aaaaaaaaaaaaaaaa\n'; fi ;;
      rm)
        [[ "$DEMO_FIXTURE_CASE" != deletion-error ]] || exit 90
        printf 'network %s\n' "${@: -1}" >>"$DEMO_FIXTURE_ROOT/deleted" ;;
      *) exit 90 ;;
    esac ;;
  volume)
    case "$2" in
      ls)
        [[ "$DEMO_FIXTURE_CASE" == already-removed || "$DEMO_FIXTURE_CASE" == partial-setup ]] ||
          printf 'telemetry-demo-aaaaaaaaaaaaaaaa-data\n' ;;
      inspect)
        if [[ "$*" == *CreatedAt* ]]; then printf '%s\n' "$DEMO_FIXTURE_CREATED"
        else printf 'telemetry-demo-aaaaaaaaaaaaaaaa\n'; fi ;;
      rm) printf 'volume %s\n' "${@: -1}" >>"$DEMO_FIXTURE_ROOT/deleted" ;;
      *) exit 90 ;;
    esac ;;
  *) exit 90 ;;
esac
exit 0
`)
			for _, tool := range []string{"kubectl", "kind", "helm", "jq"} {
				write(filepath.Join(bin, tool), "#!/bin/bash\nexit 92\n")
			}
			setupFailure := name == "daemon-unavailable" || name == "daemon-empty"
			if name != "missing" && !setupFailure {
				if err := os.Mkdir(state, 0700); err != nil {
					t.Fatal(err)
				}
				write(filepath.Join(state, "owner"), "documentdb-live-telemetry-local-v1\n")
				write(filepath.Join(state, "cluster"), "telemetry-demo-aaaaaaaaaaaaaaaa\n")
				write(filepath.Join(state, "daemon"), "demo-daemon\n")
				if name != "missing-record" {
					write(filepath.Join(state, "database.id"), strings.Repeat("a", 64)+"\n")
				}
				write(filepath.Join(state, "capture.id"), strings.Repeat("b", 64)+"\n")
				write(filepath.Join(state, "network.id"), strings.Repeat("c", 64)+"\n")
				write(filepath.Join(state, "volume.created"), "2026-09-30T12:00:00Z\n")
			}
			daemon := "demo-daemon"
			database, capture, network := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
			created := "2026-09-30T12:00:00Z"
			switch name {
			case "foreign-marker":
				write(filepath.Join(state, "owner"), "other-playground\n")
			case "foreign-name":
				write(filepath.Join(state, "cluster"), "shared-cluster\n")
			case "changed-daemon":
				daemon = "different-daemon"
			case "replaced-container":
				database = strings.Repeat("d", 64)
			case "replaced-network":
				network = strings.Repeat("d", 64)
			case "replaced-volume":
				created = "2026-09-30T13:00:00Z"
			case "symlink":
				saved := filepath.Join(root, "unrelated")
				if err := os.Rename(state, saved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(saved, state); err != nil {
					t.Fatal(err)
				}
			}
			action := "down"
			if setupFailure {
				action = "up"
			}
			command := exec.Command("bash", filepath.Join(scriptDir, "demo.sh"), action)
			command.Env = append(os.Environ(),
				"PATH="+bin+":"+os.Getenv("PATH"), "WORKSPACE_ROOT="+root,
				"DEMO_FIXTURE_ROOT="+root, "DEMO_FIXTURE_DAEMON="+daemon, "DEMO_FIXTURE_CASE="+name,
				"DEMO_FIXTURE_DATABASE="+database, "DEMO_FIXTURE_CAPTURE="+capture,
				"DEMO_FIXTURE_NETWORK="+network, "DEMO_FIXTURE_CREATED="+created,
			)
			output, err := command.CombinedOutput()
			wantSuccess := slices.Contains([]string{"owned", "missing", "already-removed", "partial-setup"}, name)
			if (err == nil) != wantSuccess {
				t.Fatalf("unexpected lifecycle result: %v\n%s", err, output)
			}
			wantDeletion := name == "owned" || name == "partial-setup" || name == "deletion-error"
			if _, err := os.Stat(filepath.Join(root, "deleted")); (err == nil) != wantDeletion {
				t.Fatalf("unexpected resource deletion for %s: %v", name, err)
			}
			if wantSuccess || setupFailure {
				if _, err := os.Stat(state); !os.IsNotExist(err) {
					t.Fatal("owned runtime state was not removed")
				}
			} else if _, err := os.Lstat(state); err != nil {
				t.Fatal("failed cleanup must preserve the ownership record")
			}
			if name == "owned" {
				deleted, err := os.ReadFile(filepath.Join(root, "deleted"))
				want := "container " + database + "\ncontainer " + capture +
					"\nvolume telemetry-demo-aaaaaaaaaaaaaaaa-data\nnetwork " + network + "\n"
				if err != nil || string(deleted) != want {
					t.Fatalf("unexpected resources removed: %s %v", deleted, err)
				}
			}
			if name == "symlink" {
				if _, err := os.Stat(filepath.Join(root, "unrelated", "owner")); err != nil {
					t.Fatal("unrelated symlink target was changed")
				}
			}
		})
	}
}
