#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail
umask 077
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
repo_root=$(git -C "$script_dir" rev-parse --show-toplevel)
state="$script_dir/.demo"
action=${1:-help}
shift "$(( $# > 0 ? 1 : 0 ))"

fail() { printf 'live telemetry demo: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || fail "Missing $1; use the repository devcontainer for tooling."; }
check_state() {
  [[ -d "$state" && ! -L "$state" && -f "$state/owner" && ! -L "$state/owner" ]] ||
    fail "No owned demo state; run up first."
  [[ "$(<"$state/owner")" == documentdb-live-telemetry-local-v1 ]] || fail "Unrecognized demo ownership marker."
}
read_state() {
  [[ -f "$state/$1" && ! -L "$state/$1" ]] || fail "Missing or unsafe demo state: $1."
  REPLY=$(<"$state/$1")
}
check_run_id() { [[ "$1" =~ ^[a-f0-9]{32}$ ]] || fail "Invalid demo run identity."; }

if [[ "$action" == help || "$action" == --help || "$action" == -h ]]; then
  printf '%s\n' \
    'Usage: bash documentdb-playground/performance-advisor/demo.sh up|run|down' \
    '  up           Start an isolated Linux/amd64 docdb-local and telemetry capture.' \
    '  run          Start a bounded Copilot observer and three gated live windows.' \
    '  run --manual Wait for an externally connected MCP observer instead.' \
    '  report       Submit one per-trial analysis JSON object on stdin (manual mode).' \
    '  down         Remove only owned demo containers, network, volume, and state.' \
    '  observe      Stdio MCP adapter, launched by the session-only MCP config.' \
    'Host invocations use the matching running devcontainer; nothing is installed.'
  exit 0
fi
case "$action" in up|run|down|observe|report|_workload|_transcript|_mcp_names) ;; *) fail "Unknown command: $action." ;; esac
[[ $# -le 1 ]] || fail "Too many arguments."
if [[ "$action" == down && ! -e "$state" && ! -L "$state" ]]; then
  printf 'No owned demo state exists; no resources were touched.\n'
  exit 0
fi

# Keep Copilot where it is already installed; send repository tooling to the container.
if [[ "$action" == run ]]; then
  [[ $# -eq 0 || "$1" == --manual ]] || fail "run accepts only --manual."
  manual=${1:-}
  check_state
  [[ -f "$state/up-complete" ]] || fail "Setup did not finish; inspect up output and run down."
  need timeout
  if [[ -z "$manual" ]]; then
    command -v copilot >/dev/null ||
      fail "Copilot CLI is not installed here. Run from your host terminal, or use run --manual."
  fi
  need od
  run_id=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
  check_run_id "$run_id"
  run_dir="$state/run-$run_id"
  mkdir -m 700 -- "$run_dir"
  workload_pid=
  observer_pid=
  output_pid=
  finished=false
  cleanup_run() {
    local rc=$?
    trap - EXIT INT TERM
    if [[ "$finished" != true ]]; then
      touch -- "$run_dir/cancel"
      if [[ -n "$observer_pid" ]] && kill -0 "$observer_pid" 2>/dev/null; then
        kill -TERM "$observer_pid"
      fi
      if [[ -n "$output_pid" ]] && kill -0 "$output_pid" 2>/dev/null; then
        kill -TERM "$output_pid"
      fi
    fi
    if [[ -n "$observer_pid" ]]; then wait "$observer_pid" || :; fi
    if [[ -n "$output_pid" ]]; then wait "$output_pid" || :; fi
    if [[ -n "$workload_pid" ]]; then wait "$workload_pid" || :; fi
    if [[ "$rc" -ne 0 ]]; then
      printf 'Demo did not pass. Workload details: %s/workload.log\nRun down to remove the owned local runtime.\n' "$run_dir" >&2
      if [[ -f "$run_dir/workload.log" ]]; then tail -30 "$run_dir/workload.log" >&2; fi
    fi
    exit "$rc"
  }
  trap cleanup_run EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  observer_mcp_args=(--enable-mcp-server=live-telemetry)
  if [[ -z "$manual" ]]; then
    if ! (cd "$repo_root" && copilot mcp list --json) |
      bash "$script_dir/demo.sh" _mcp_names >"$run_dir/mcp-server-names"; then
      fail "Could not enumerate and isolate the observer's MCP servers."
    fi
    while IFS= read -r -d '' server; do
      observer_mcp_args+=("--disable-mcp-server=$server")
    done <"$run_dir/mcp-server-names"
  fi
  bash "$script_dir/demo.sh" _workload "$run_id" >"$run_dir/workload.log" 2>&1 &
  workload_pid=$!
  for ((attempt=0; attempt<120; attempt++)); do
    [[ ! -f "$run_dir/run.json" ]] || break
    kill -0 "$workload_pid" 2>/dev/null || fail "Workload preparation failed."
    sleep 1
  done
  [[ -f "$run_dir/run.json" ]] || fail "MCP preparation timed out."
  printf 'Observer readiness gate armed. Three 150-second windows; each requires broad signal coverage, three trace timelines, and a live report.\n'
  if [[ -n "$manual" ]]; then
    printf 'Connect an MCP client using %s/demo-mcp.json and skill/SKILL.md.\nSubmit each analysis JSON before its deadline with: bash documentdb-playground/performance-advisor/demo.sh report < report.json\n' "$script_dir"
  else
    prompt="Investigate the bounded synthetic telemetry demo using these instructions:

$(<"$script_dir/skill/SKILL.md")

Run the packaged live demo now. Use only the four live-telemetry tools.
Make tool calls one at a time; the bounded adapter does not allow parallel reads.
Use ASCII punctuation; do not use em-dashes.
Start with get_capture_status, which releases the readiness gate.
Follow the appended neutral schedule. There are three 150-second trials.
After the first 30 seconds of each trial, read capture status for its inventory.
Read every metric named in required_coverage using lookback_seconds=45 and
limit=60. A single duration read covers request_duration and backend_phases.
Inspect operations/error dimensions, duration/phase totals or histogram
populations, request/response bytes, returned documents, health and any available
resource metrics. Fetch THREE distinct current Find traces: get_recent_traces
with limit=5, then get_trace with limit=24 for each ID. Do not reuse old traces.
Explain complete received timelines including pre-execute, acquisition overlap,
execution, response writing, and phases absent from the examples.
Use the appended required_coverage and live_reads. Retry fresh data if a row is
pending. Do not fabricate averages or percentile estimates from separate sums.
When a duration or size metric has an empty unit, report its raw value and
missing unit. Do not infer seconds or bytes from its name or another series.
Before each deadline, emit exactly one DEMO_REPORT JSON line using the schema
in the skill. Copy all current evidence_ids for every required_coverage row.
Keep each observation, interpretation, and limitation brief but substantive.
The launcher validates and displays that report. Reads alone do not pass.
After emitting it, use status to confirm report_accepted=true and wait.
Poll get_capture_status for schedule changes, staying below 128 total calls.
Do not finish after trial-1 or trial-2. Stop on completed or failed, or stop_by.
On completed, return a short final comparison and the evidence limitations.
Do not read repository files, discover fault mappings, invoke other tools,
or start background agents. This user-requested run is bounded to 16 minutes."
    cd "$repo_root"
    need mkfifo
    mkfifo "$run_dir/observer.pipe"
    bash "$script_dir/demo.sh" _transcript "$run_id" <"$run_dir/observer.pipe" &
    output_pid=$!
    timeout --signal=TERM --kill-after=10s 16m copilot \
      --no-custom-instructions --disable-builtin-mcps --no-ask-user --no-auto-update \
      "${observer_mcp_args[@]}" \
      --additional-mcp-config="@$script_dir/demo-mcp.json" \
      --available-tools=mcp:live-telemetry-get_capture_status,mcp:live-telemetry-get_metric_window,mcp:live-telemetry-get_recent_traces,mcp:live-telemetry-get_trace \
      --allow-tool=live-telemetry --stream=on --silent --log-level=error \
      --prompt="$prompt" >"$run_dir/observer.pipe" &
    observer_pid=$!
    wait "$observer_pid"
    observer_pid=
    wait "$output_pid"
    output_pid=
    rm -- "$run_dir/observer.pipe"
  fi
  wait "$workload_pid"
  workload_pid=
  if [[ -f "$run_dir/observer.txt" ]]; then
    cp -- "$run_dir/observer.txt" "$script_dir/evidence/latest-observer.txt"
  fi
  cp -- "$state/images.json" "$script_dir/evidence/latest-images.json"
  cp -- "$state/version.txt" "$script_dir/evidence/latest-version.txt"
  finished=true
  printf 'Live coverage and report gates passed. Evidence and interpretations: %s/evidence/latest-demo.json\n' "$script_dir"
  exit 0
fi

if [[ ! -e /.dockerenv && "${WORKSPACE_ROOT:-}" != "$repo_root" ]]; then
  need docker
  mapfile -t containers < <(docker ps \
    --filter "label=devcontainer.config_file=$repo_root/.devcontainer/devcontainer.json" --format '{{.ID}}')
  if [[ ${#containers[@]} -eq 0 ]]; then
    mapfile -t candidates < <(docker ps --filter label=devcontainer.config_file --format '{{.ID}}')
    for candidate in "${candidates[@]}"; do
      if docker inspect "$candidate" --format '{{range .Mounts}}{{println .Source}}{{end}}' |
        grep -Fqx -- "$repo_root"; then containers+=("$candidate"); fi
    done
  fi
  [[ ${#containers[@]} -eq 1 ]] ||
    fail "Expected one running devcontainer for this checkout; found ${#containers[@]}. Start or disambiguate it."
  container=${containers[0]}
  workspace=$(docker inspect "$container" --format "{{range .Mounts}}{{if eq .Source \"$repo_root\"}}{{println .Destination}}{{end}}{{end}}")
  [[ -n "$workspace" && "$workspace" != *$'\n'* ]] || fail "Could not resolve the shared workspace."
  if [[ "$action" == observe || "$action" == report || "$action" == _mcp_names ]]; then
    exec docker exec -i --user vscode --workdir "$workspace" "$container" \
      bash "$workspace/documentdb-playground/performance-advisor/demo.sh" "$action" "$@"
  fi
  exec bash "$script_dir/demo-transport.sh" "$container" "$workspace" "$action" "$@"
fi

[[ "$(id -u)" -ne 0 ]] || fail "Run tooling as the devcontainer's non-root vscode user."
cd "$repo_root"
if [[ "$action" == _mcp_names ]]; then
  [[ $# -eq 0 ]] || fail "_mcp_names takes no arguments."
  need jq
  exec jq -s -j '
    if length != 1 then error("expected one MCP inventory object")
    else .[0] |
      if type != "object" or (.mcpServers | type) != "object" then
        error("expected an mcpServers object")
      else .mcpServers | keys[] | select(. != "live-telemetry") |
        if (explode | index(0)) != null then error("invalid MCP server name")
        else . + "\u0000" end
      end
    end'
fi
if [[ "$action" == observe || "$action" == report || "$action" == _transcript ]]; then
  check_state
  observer_action=observe
  if [[ "$action" == _transcript ]]; then
    [[ $# -eq 1 ]] || fail "_transcript requires one run identity."
    REPLY=$1
    observer_action=transcript
  else
    [[ $# -eq 0 ]] || fail "$action takes no arguments."
    read_state current-run
    if [[ "$action" == report ]]; then observer_action=submit; fi
  fi
  check_run_id "$REPLY"
  exec "$state/demo-observer" --action "$observer_action" --dir "$state/run-$REPLY"
fi
need flock
[[ ! -L "$state" && ! -L "$script_dir/.demo.lock" ]] || fail "Refusing symlinked lifecycle state."
exec 9>"$script_dir/.demo.lock"
flock --exclusive --nonblock 9 || fail "Another lifecycle command is active; stop run before down."
for tool in docker jq; do need "$tool"; done

owner_label=documentdb.live-telemetry.owner

# Validate every resource before deleting any. Names alone never authorize deletion.
verify_resource() {
  local kind=$1 name=$2 record=$3 actual expected observed label_format
  case "$kind" in
    container)
      actual=$(docker container ls --all --no-trunc --filter "name=^/$name$" --format '{{.ID}}')
      label_format="{{index .Config.Labels \"$owner_label\"}}"
      ;;
    network)
      actual=$(docker network ls --no-trunc --filter "name=^$name$" --format '{{.ID}}')
      label_format="{{index .Labels \"$owner_label\"}}"
      ;;
    volume)
      actual=$(docker volume ls --filter "name=^$name$" --format '{{.Name}}')
      label_format="{{index .Labels \"$owner_label\"}}"
      ;;
    *) fail "Unknown resource kind." ;;
  esac
  [[ "$actual" != *$'\n'* ]] || fail "Ambiguous owned $kind."
  if [[ -z "$actual" ]]; then
    if [[ "$kind" == container && -e "$state/$record" ]]; then
      read_state "$record"
      [[ "$REPLY" =~ ^[a-f0-9]{64}$ ]] || fail "Invalid recorded container identity."
      [[ -z "$(docker container ls --all --no-trunc --filter "id=$REPLY" --format '{{.ID}}')" ]] ||
        fail "Recorded container was renamed; refusing cleanup."
    fi
    REPLY=
    return
  fi
  read_state "$record"
  expected=$REPLY
  observed=$actual
  if [[ "$kind" == volume ]]; then
    [[ "$actual" == "$name" ]] || fail "Unexpected volume name."
    observed=$(docker volume inspect --format '{{.CreatedAt}}' "$actual")
  else
    [[ "$expected" =~ ^[a-f0-9]{64}$ ]] || fail "Invalid recorded $kind identity."
  fi
  [[ -n "$expected" && "$observed" == "$expected" ]] || fail "The $kind no longer matches this demo's ownership record."
  [[ "$(docker "$kind" inspect --format "$label_format" "$actual")" == "$cluster" ]] ||
    fail "The $kind ownership label changed."
  REPLY=$actual
}

if [[ "$action" == up ]]; then
  [[ $# -eq 0 ]] || fail "up takes no arguments."
  [[ ! -e "$state" ]] || fail "Demo state already exists; run down before creating a fresh environment."
  for tool in go openssl; do need "$tool"; done
  [[ "$(uname -sm)" == "Linux x86_64" ]] || fail "This pinned demo requires Linux/amd64."
  daemon=$(docker info --format '{{.ID}}')
  [[ -n "$daemon" ]] || fail "Docker did not provide a daemon identity."
  cluster="telemetry-demo-$(openssl rand -hex 8)"
  [[ "$cluster" =~ ^telemetry-demo-[a-f0-9]{16}$ ]] || fail "Could not create a runtime identity."
  mkdir -m 700 -- "$state"
  printf 'documentdb-live-telemetry-local-v1\n' >"$state/owner"
  mkdir -m 700 -- "$state/docker"
  export DOCKER_CONFIG="$state/docker"
  printf '%s\n' "$cluster" >"$state/cluster"
  printf '%s\n' "$daemon" >"$state/daemon"
  verify_resource container "$cluster-database" database.id
  verify_resource container "$cluster-capture" capture.id
  verify_resource network "$cluster" network.id
  verify_resource volume "$cluster-data" volume.created
  trap 'exit 130' INT
  trap 'exit 143' TERM
  image=ghcr.io/documentdb/documentdb/documentdb-local@sha256:7b0e9a510476ab42bc2956c40d89c39062cdef23fc3509b9b510eb894138812c
  printf 'Building the observer and standalone workload for %s (docdb-local, no Kubernetes).\n' "$cluster"
  (cd "$script_dir" && go build -o "$state/" ./cmd/demo-observer ./cmd/demo-workload)
  docker build -t "live-telemetry-tool:$cluster" "$script_dir"
  docker pull --quiet "$image"
  docker run --rm --network none --entrypoint cat "$image" /version.txt >"$state/version.txt"
  docker network create --internal --label "$owner_label=$cluster" "$cluster" >"$state/network.id"
  gateway=$(docker network inspect "$cluster" --format '{{(index .IPAM.Config 0).Gateway}}')
  [[ "$gateway" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || fail "Expected an IPv4 Docker bridge."
  docker create --name "$cluster-capture" --label "$owner_label=$cluster" \
    --network host --read-only --cap-drop ALL --security-opt no-new-privileges \
    --memory 256m --pids-limit 64 --log-opt max-size=5m --log-opt max-file=1 \
    "live-telemetry-tool:$cluster" --namespace live-telemetry --cluster telemetry-db \
    --otlp-address "$gateway:0" --mcp-address 127.0.0.1:0 >"$state/capture.id"
  capture=$(<"$state/capture.id")
  docker start "$capture" >/dev/null
  listeners=
  for ((attempt=0; attempt<100; attempt++)); do
    [[ "$(docker inspect --format '{{.State.Running}}' "$capture")" == true ]] || fail "Capture stopped; inspect its container logs."
    docker logs "$capture" >"$state/capture.log" 2>&1
    if listeners=$(jq -s -er '[.[] | select(.msg == "capture listeners ready")] | last |
      select(.otlp != null and .mcp != null) | [.otlp, .mcp] | @tsv' "$state/capture.log"); then break; fi
    sleep 0.1
  done
  [[ -n "$listeners" ]] || fail "Capture listeners did not become ready."
  read -r otlp mcp <<<"$listeners"
  [[ "$otlp" == "$gateway:"* && "$mcp" =~ ^127\.0\.0\.1:[1-9][0-9]*$ ]] || fail "Unexpected capture bindings."
  printf 'http://%s/mcp\n' "$mcp" >"$state/endpoint"
  docker run --rm --network none --entrypoint cat "$image" \
    /home/documentdb/gateway/pg_documentdb_gw/SetupConfiguration.json |
    jq --arg endpoint "http://$otlp" '.TelemetryOptions.Metrics = {
      Enabled:true, OtlpEndpoint:$endpoint, ExportIntervalMs:15000, ExportTimeoutMs:10000
    } | .TelemetryOptions.Tracing = {
      Enabled:true, OtlpEndpoint:$endpoint, SamplerRatio:1.0, ExportTimeoutMs:10000
    }' >"$state/SetupConfiguration.json"
  chmod 644 "$state/SetupConfiguration.json"
  openssl rand -hex 24 >"$state/password"
  {
    printf 'USERNAME=telemetry_user\nPASSWORD=%s\n' "$(<"$state/password")"
    printf 'ENABLE_TELEMETRY=false\nSKIP_INIT_DATA=true\nTLS_MODE=requireTLS\n'
    printf 'CONFIG_DIR=/demo-config\n'
    printf 'OTEL_RESOURCE_ATTRIBUTES=service.namespace=live-telemetry,documentdb.cluster=telemetry-db,service.instance.id=%s-gateway\n' "$cluster"
  } >"$state/database.env"
  docker volume create --label "$owner_label=$cluster" "$cluster-data" >/dev/null
  docker volume inspect --format '{{.CreatedAt}}' "$cluster-data" >"$state/volume.created"
  docker create --name "$cluster-database" --label "$owner_label=$cluster" \
    --network "$cluster" --env-file "$state/database.env" \
    --mount "type=volume,src=$cluster-data,dst=/data" \
    --mount "type=bind,src=$state/SetupConfiguration.json,dst=/demo-config/SetupConfiguration.json,readonly" \
    --memory 2g --pids-limit 256 --log-opt max-size=5m --log-opt max-file=1 \
    "$image" >"$state/database.id"
  database=$(<"$state/database.id")
  docker start "$database" >/dev/null
  address=$(docker inspect "$database" --format "{{(index .NetworkSettings.Networks \"$cluster\").IPAddress}}"):10260
  [[ "$address" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}:10260$ ]] || fail "Expected an IPv4 private database address."
  printf '%s\n' "$address" >"$state/address"
  "$state/demo-workload" --action ready --address "$address" --password-file "$state/password"
  docker inspect "$capture" "$database" |
    jq '[.[] | {name:.Name,id:.Id,image:.Config.Image,image_id:.Image}]' >"$state/images.json"
  touch "$state/up-complete"
  printf 'docdb-local demo ready: %s. No Kubernetes or Collector is running for this demo.\nRun demo.sh run from the terminal with your authenticated Copilot CLI.\n' "$cluster"
  exit 0
fi

check_state
read_state cluster
cluster=$REPLY
[[ "$cluster" =~ ^telemetry-demo-[a-f0-9]{16}$ ]] || fail "Refusing an unrecognized runtime name."
read_state daemon
[[ "$(docker info --format '{{.ID}}')" == "$REPLY" ]] || fail "Docker daemon changed; refusing to operate on another environment."
verify_resource container "$cluster-database" database.id
database=$REPLY
verify_resource container "$cluster-capture" capture.id
capture=$REPLY
verify_resource network "$cluster" network.id
network=$REPLY
verify_resource volume "$cluster-data" volume.created
volume=$REPLY

if [[ "$action" == down ]]; then
  [[ $# -eq 0 ]] || fail "down takes no arguments."
  if [[ -n "$database" ]]; then docker container rm --force "$database" >/dev/null; fi
  if [[ -n "$capture" ]]; then docker container rm --force "$capture" >/dev/null; fi
  if [[ -n "$volume" ]]; then docker volume rm "$volume" >/dev/null; fi
  if [[ -n "$network" ]]; then docker network rm "$network" >/dev/null; fi
  [[ "$(realpath "$state")" == "$script_dir/.demo" ]] || fail "Runtime state moved; refusing cleanup."
  rm -rf -- "$state"
  printf 'Owned docdb-local containers, volume, network, and private state removed; image cache and selected evidence preserved.\n'
  exit 0
fi

[[ "$action" == _workload && $# -eq 1 ]] || fail "Invalid workload invocation."
check_run_id "$1"
run_id=$1
run_dir="$state/run-$run_id"
[[ -d "$run_dir" && ! -L "$run_dir" && -n "$database" && -n "$capture" && -n "$network" && -n "$volume" ]] ||
  fail "Missing run directory or owned local runtime."
[[ -f "$state/up-complete" ]] || fail "Demo setup is incomplete."
[[ "$(docker inspect --format '{{.State.Running}}' "$database")" == true &&
   "$(docker inspect --format '{{.State.Running}}' "$capture")" == true ]] || fail "Owned demo containers are not running."
read_state address
address=$REPLY
read_state endpoint
endpoint=$REPLY
workload_pid=
cleanup_workload() {
  local rc=$?
  trap - EXIT INT TERM
  touch "$run_dir/cancel"
  if [[ -n "$workload_pid" ]] && kill -0 "$workload_pid" 2>/dev/null; then
    kill -TERM "$workload_pid"
    wait "$workload_pid" || :
  fi
  exit "$rc"
}
trap cleanup_workload EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
printf '%s\n' "$run_id" >"$state/current-run"
"$state/demo-observer" --action prepare --dir "$run_dir" --endpoint "$endpoint"
timeout --signal=TERM --kill-after=10s 16m "$state/demo-workload" \
  --address "$address" --password-file "$state/password" --dir "$run_dir" &
workload_pid=$!
wait "$workload_pid"
workload_pid=
for ((attempt=0; attempt<60; attempt++)); do
  if jq -e '.disconnected == true' "$run_dir/observer.json" >/dev/null; then break; fi
  sleep 1
done
"$state/demo-observer" --action report --dir "$run_dir" --output "$script_dir/evidence/latest-demo.json"
